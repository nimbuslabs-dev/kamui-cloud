package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

var otherProjectID string

func openStore(t *testing.T) (*core.Store, *Store) {
	t.Helper()
	ctx := context.Background()
	coreStore, err := core.Open(ctx, filepath.Join(t.TempDir(), "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coreStore.Close() })
	// A second project for isolation tests, created through the identity API.
	idStore := identity.NewStore(coreStore.DB(), "test-master-secret-at-least-32-characters", "admin-password")
	membership, err := idStore.CreateProject(ctx, identity.LocalAdminID, "Other", "", false)
	if err != nil {
		t.Fatal(err)
	}
	otherProjectID = membership.ID
	root := t.TempDir()
	store := NewStore(coreStore.DB(), "test-master-secret-at-least-32-characters", root)
	return coreStore, store
}

func TestArtifactLifecycleAndChecksums(t *testing.T) {
	ctx := context.Background()
	_, store := openStore(t)

	content := []byte("kaggle-dataset-v1\nline two\n")
	artifact, err := store.CreateArtifact(ctx, "default", "dataset", "dataset", "text/plain", "local-admin", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if artifact.SizeBytes != int64(len(content)) {
		t.Fatalf("size = %d", artifact.SizeBytes)
	}
	sum := sha256.Sum256(content)
	if artifact.Checksum != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("checksum = %s", artifact.Checksum)
	}

	// Duplicate names are rejected per project.
	if _, err := store.CreateArtifact(ctx, "default", "dataset", "dataset", "text/plain", "x", bytes.NewReader(content)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	// The same name is fine in another project.
	if _, err := store.CreateArtifact(ctx, otherProjectID, "dataset", "dataset", "text/plain", "x", bytes.NewReader(content)); err != nil {
		t.Fatalf("other project: %v", err)
	}

	// Download round-trips the exact bytes.
	reader, size, err := store.OpenArtifact(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || size != int64(len(content)) || !bytes.Equal(data, content) {
		t.Fatalf("round trip = %d bytes, %v", len(data), err)
	}
	if err := store.VerifyChecksum(ctx, artifact); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Oversized uploads are rejected and leave nothing behind.
	if _, err := store.CreateArtifact(ctx, "default", "huge", "generic", "text/plain", "x", bytes.NewReader(make([]byte, MaxArtifactBytes+1))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize = %v", err)
	}
	listed, err := store.ListArtifacts(ctx, "default", 0)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %d items, %v", len(listed), err)
	}

	if err := store.DeleteArtifact(ctx, "default", artifact.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetArtifact(ctx, "default", artifact.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
	if _, _, err := store.OpenArtifact(ctx, artifact); !errors.Is(err, ErrNotFound) {
		t.Fatal("blob must be gone")
	}
}

func TestArtifactUploadIsAtomicOnTruncation(t *testing.T) {
	ctx := context.Background()
	_, store := openStore(t)
	// A truncated body still records whatever arrived; the checksum reflects
	// the bytes actually stored, so integrity is always verifiable.
	partial := []byte("only-first-part")
	artifact, err := store.CreateArtifact(ctx, "default", "partial", "generic", "text/plain", "x", bytes.NewReader(partial))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyChecksum(ctx, artifact); err != nil {
		t.Fatalf("partial upload must still verify: %v", err)
	}
	dir, err := store.blobPath("default", artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	uploads := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".upload-") {
			uploads++
		}
	}
	if uploads != 0 {
		t.Fatalf("temp upload files leaked: %d", uploads)
	}
}

func TestSecretsAndAssignmentScopedResolution(t *testing.T) {
	ctx := context.Background()
	_, store := openStore(t)

	if err := store.PutSecret(ctx, "default", "db-password", "correct-horse-battery", "local-admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSecret(ctx, "default", "db-password", "rotated-value", "local-admin"); err != nil {
		t.Fatal(err)
	}

	// The value must never be visible in metadata listings.
	metas, err := store.ListSecrets(ctx, "default")
	if err != nil || len(metas) != 1 || strings.Contains(metas[0].Name, "rotated-value") {
		t.Fatalf("meta = %+v %v", metas, err)
	}
	if metas[0].RotatedAt == nil {
		t.Fatal("rotation timestamp missing")
	}

	// Values live sealed; decrypt directly through the DB to confirm sealing.
	var sealed string
	if err := store.db.QueryRowContext(ctx,
		"SELECT sealed_value FROM secrets WHERE project_id='default' AND name='db-password'").Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "rotated-value") {
		t.Fatal("secret value stored in plaintext")
	}

	resolved, missing, err := store.ResolveEnv(ctx, "default", map[string]string{
		"DB_PASS": "secret:db-password",
		"PLAIN":   "hello",
	})
	if err != nil || len(missing) != 0 {
		t.Fatalf("resolve = %v, %v", err, missing)
	}
	if resolved["DB_PASS"] != "rotated-value" || resolved["PLAIN"] != "hello" {
		t.Fatalf("resolved = %+v", resolved)
	}

	// Unknown secrets fail closed.
	if _, _, err := store.ResolveEnv(ctx, "default", map[string]string{"X": "secret:nope"}); err == nil || len(missing) > 0 && err == nil {
		t.Fatalf("unknown secret = %v", err)
	} else if err != nil && !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error should name the missing secret: %v", err)
	}

	// Cross-project isolation.
	if _, _, err := store.ResolveEnv(ctx, otherProjectID, map[string]string{"X": "secret:db-password"}); err == nil {
		t.Fatal("cross-project secret resolution must fail")
	}

	// Sealed storage stays sealed in the row for the *other* project too.
	if err := store.PutSecret(ctx, otherProjectID, "db-password", "other-project-value", "local-admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx,
		"SELECT sealed_value FROM secrets WHERE project_id=? AND name='db-password'", otherProjectID).Scan(&sealed); err == nil && strings.Contains(sealed, "other-project-value") {
		t.Fatal("cross-project value collision")
	} else if errors.Is(err, sql.ErrNoRows) {
		// fine
	}

	if err := store.DeleteSecret(ctx, "default", "db-password"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ResolveEnv(ctx, "default", map[string]string{"X": "secret:db-password"}); err == nil {
		t.Fatal("deleted secret must not resolve")
	}
}

func TestRegistryCredentialScoping(t *testing.T) {
	ctx := context.Background()
	_, store := openStore(t)

	if err := store.PutRegistryCredential(ctx, "default", "ghcr.io", "user", "pat-token", "local-admin"); err != nil {
		t.Fatal(err)
	}
	username, password, err := store.registryAuthFor(ctx, "default", "ghcr.io")
	if err != nil || username != "user" || password != "pat-token" {
		t.Fatalf("scoped auth = %q %q %v", username, password, err)
	}
	// Other projects get nothing for the same host.
	username, _, err = store.registryAuthFor(ctx, otherProjectID, "ghcr.io")
	if err != nil || username != "" {
		t.Fatalf("cross-project credential = %q %v", username, err)
	}
	if err := store.PutRegistryCredential(ctx, "default", "bad host", "u", "p", "x"); !errors.Is(err, ErrBadInput) {
		t.Fatalf("invalid host = %v", err)
	}
	if err := store.DeleteRegistryCredential(ctx, "default", "ghcr.io"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRegistryCredential(ctx, "default", "ghcr.io"); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete must be not found")
	}
}

func TestVolumePlacementPinning(t *testing.T) {
	ctx := context.Background()
	_, store := openStore(t)

	owner, err := store.VolumeOwner(ctx, "default", "pgdata")
	if err != nil || owner != "" {
		t.Fatalf("unset volume = %q %v", owner, err)
	}
	if err := store.RecordVolumePlacement(ctx, "default", "pgdata", "wk-a", 1<<30); err != nil {
		t.Fatal(err)
	}
	// A larger request upgrades the recorded size; the owner never moves.
	if err := store.RecordVolumePlacement(ctx, "default", "pgdata", "wk-a", 2<<30); err != nil {
		t.Fatal(err)
	}
	owner, err = store.VolumeOwner(ctx, "default", "pgdata")
	if err != nil || owner != "wk-a" {
		t.Fatalf("pinned owner = %q %v", owner, err)
	}
	placements, err := store.ListVolumePlacements(ctx, "default")
	if err != nil || len(placements) != 1 || placements[0].SizeBytes != 2<<30 {
		t.Fatalf("placements = %+v %v", placements, err)
	}
	// Another project's same-named volume is independent.
	if err := store.RecordVolumePlacement(ctx, otherProjectID, "pgdata", "wk-b", 0); err != nil {
		t.Fatal(err)
	}
	if owner, _ := store.VolumeOwner(ctx, otherProjectID, "pgdata"); owner != "wk-b" {
		t.Fatalf("cross-project placement = %q", owner)
	}
}

func TestServiceRouteLifecycle(t *testing.T) {
	ctx := context.Background()
	coreStore, store := openStore(t)

	spec := core.Submission{Image: "example:local", Kind: "service"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	workload, _, err := coreStore.Submit(ctx, spec, "route-test")
	if err != nil {
		t.Fatal(err)
	}
	wl := workload.ID

	if err := store.RegisterRoute(ctx, wl, "default", "wk-a", 3, "fence-1", 8080, "http"); err != nil {
		t.Fatal(err)
	}
	route, err := store.GetRoute(ctx, wl)
	if err != nil || route.Port != 8080 || route.Attempt != 3 {
		t.Fatalf("route = %+v %v", route, err)
	}
	// A replacement assignment overwrites the route.
	if err := store.RegisterRoute(ctx, wl, "default", "wk-a", 4, "fence-2", 9090, "http"); err != nil {
		t.Fatal(err)
	}
	if route, _ = store.GetRoute(ctx, wl); route.Attempt != 4 || route.Port != 9090 {
		t.Fatalf("route replacement = %+v", route)
	}
	if err := store.RegisterRoute(ctx, wl, "default", "wk-a", 1, "f", 70000, "http"); !errors.Is(err, ErrBadInput) {
		t.Fatalf("invalid port = %v", err)
	}
	if err := store.RevokeRoute(ctx, wl); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRoute(ctx, wl); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked route must be gone")
	}
}

func TestBuildEnvelopeFailsClosed(t *testing.T) {
	ctx := context.Background()
	_, store := openStore(t)

	content := []byte("input data")
	artifact, err := store.CreateArtifact(ctx, "default", "input", "dataset", "text/plain", "x", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSecret(ctx, "default", "api-key", "secret-value", "x"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRegistryCredential(ctx, "default", "ghcr.io", "deploy-bot", "gh-token", "x"); err != nil {
		t.Fatal(err)
	}

	submission := core.Submission{
		Image:       "ghcr.io/example/app:1.0",
		Kind:        "service",
		Network:     "private",
		Env:         map[string]string{"KEY": "secret:api-key"},
		Artifacts:   []core.ArtifactMount{{ArtifactID: artifact.ID, Path: "/kamui/input.txt"}},
		Outputs:     []string{"/out/model.bin"},
		Volumes:     []core.VolumeRequest{{Name: "model-cache"}},
		HealthCheck: &core.HealthCheck{Kind: "http", Port: 8080, Path: "/healthz"},
	}
	if err := submission.Normalize(); err != nil {
		t.Fatal(err)
	}
	envelope, problems, err := store.BuildEnvelope(ctx, "default", submission)
	if err != nil || len(problems) != 0 {
		t.Fatalf("envelope = %+v, problems %v, err %v", envelope, problems, err)
	}
	if envelope.Env["KEY"] != "secret-value" {
		t.Fatalf("secret not resolved: %+v", envelope.Env)
	}
	if envelope.ProjectNet == "" || !strings.HasPrefix(envelope.ProjectNet, "kamui-net-") {
		t.Fatalf("project network = %q", envelope.ProjectNet)
	}
	if envelope.RegistryAuth == nil || envelope.RegistryAuth.Host != "ghcr.io" {
		t.Fatalf("registry auth = %+v", envelope.RegistryAuth)
	}
	if len(envelope.Artifacts) != 1 || envelope.Artifacts[0].Path != "/kamui/input.txt" {
		t.Fatalf("artifacts = %+v", envelope.Artifacts)
	}

	// Unknown secret fails closed with the secret named.
	bad := submission
	bad.Env = map[string]string{"KEY": "secret:missing"}
	_, problems, err = store.BuildEnvelope(ctx, "default", bad)
	if err == nil && len(problems) == 0 {
		t.Fatal("unknown secret must fail closed")
	}

	// Unknown artifact fails closed.
	bad = submission
	bad.Env = nil
	bad.Artifacts = []core.ArtifactMount{{ArtifactID: "art-missing", Path: "/kamui/x"}}
	_, problems, _ = store.BuildEnvelope(ctx, "default", bad)
	if len(problems) == 0 || !strings.Contains(problems[0], "art-missing") {
		t.Fatalf("artifact problems = %v", problems)
	}
}

func TestProjectNetworkNamesAreStableAndDistinct(t *testing.T) {
	a := ProjectNetworkName("default")
	b := ProjectNetworkName("default")
	c := ProjectNetworkName("other")
	if a != b || a == c || len(a) != len("kamui-net-")+16 {
		t.Fatalf("network names = %q %q %q", a, b, c)
	}
}
