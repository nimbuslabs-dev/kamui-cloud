package deploy

import (
	"context"
	"crypto/hmac"
	"errors"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"strconv"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

func openDeployStore(t *testing.T) (*core.Store, *Store) {
	t.Helper()
	ctx := context.Background()
	coreStore, err := core.Open(ctx, filepath.Join(t.TempDir(), "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coreStore.Close() })
	idStore := identity.NewStore(coreStore.DB(), "test-master-secret-at-least-32-characters", "admin-password")
	if err := idStore.EnsureAdminBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	return coreStore, NewStore(coreStore.DB(), idStore, coreStore, nil)
}

func TestDeploymentLifecycleWithoutBuilder(t *testing.T) {
	ctx := context.Background()
	coreStore, store := openDeployStore(t)

	deployment, err := store.CreateDeployment(ctx, "default", "hello-api", "image", "busybox:latest", "",
		core.PlacementAuto, "", core.Resources{CPUMillis: 250, MemoryBytes: 64 << 20}, nil, "local-admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if deployment.State != "created" || deployment.SourceKind != "image" {
		t.Fatalf("deployment = %+v", deployment)
	}
	if _, err := store.CreateDeployment(ctx, "default", "hello-api", "image", "busybox:latest", "", "", "", core.Resources{}, nil, "x"); !IsConflict(err) {
		t.Fatalf("duplicate name = %v", err)
	}

	// Source validation.
	if _, err := store.CreateDeployment(ctx, "default", "bad-source", "git", "http://github.com/a/b", "", "", "", core.Resources{}, nil, "x"); err == nil {
		t.Fatal("non-https git source must be rejected")
	}
	if _, err := store.CreateDeployment(ctx, "default", "bad-source2", "git", "https://evil.example/a/b", "", "", "", core.Resources{}, nil, "x"); err == nil {
		t.Fatal("unauthorized git host must be rejected")
	}
	if err := ValidateGitSource("https://github.com/owner/repo"); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	if err := ValidateGitSource("https://github.com/owner/repo?query=1"); err == nil {
		t.Fatal("query strings must be rejected")
	}

	// TriggerBuild without a builder fails clearly instead of hanging.
	if _, err := store.TriggerBuild(ctx, deployment, "", "", nil, "local-admin"); err == nil {
		t.Fatal("build without a builder must fail with a clear error")
	}

	// No revision exists yet, so releasing one fails with not-found.
	if _, err := store.ReleaseRevision(ctx, deployment, 1, "local-admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("releasing a missing revision = %v", err)
	}
	_ = coreStore
}

func IsConflict(err error) bool { return err != nil && strings.Contains(err.Error(), "exists") }

func TestWebhookSignatureValidation(t *testing.T) {
	ctx := context.Background()
	_, store := openDeployStore(t)

	deployment, err := store.CreateDeployment(ctx, "default", "hooked", "git", "https://github.com/owner/repo", "",
		core.PlacementAuto, "", core.Resources{}, nil, "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	webhook, secret, err := store.CreateWebhook(ctx, "default", deployment.ID, "main", "local-admin")
	if err != nil || secret == "" {
		t.Fatalf("webhook: %v", err)
	}
	listed, err := store.ListWebhooks(ctx, deployment.ID)
	if err != nil || len(listed) != 1 || listed[0].Secret != "" {
		t.Fatalf("webhook secret must never be listed: %+v", listed)
	}

	sign := func(body, ts string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts))
		mac.Write([]byte(body))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	body := `{"branch":"main","commit":""}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	trigger, err := store.MatchWebhook(ctx, webhook.ID, sign(body, ts), ts, body, "main")
	if err != nil {
		t.Fatalf("valid delivery rejected: %v", err)
	}
	if trigger.Deployment.ID != deployment.ID {
		t.Fatalf("trigger deployment = %+v", trigger)
	}

	// Wrong signature.
	if _, err := store.MatchWebhook(ctx, webhook.ID, sign(body, ts)+"0", ts, body, "main"); err == nil {
		t.Fatal("bad signature must be rejected")
	}
	// Stale timestamp (replay).
	old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	if _, err := store.MatchWebhook(ctx, webhook.ID, sign(body, old), old, body, "main"); err == nil {
		t.Fatal("replayed delivery must be rejected")
	}
	// Branch filter mismatch.
	if _, err := store.MatchWebhook(ctx, webhook.ID, sign(body, ts), ts, body, "feature"); err == nil {
		t.Fatal("branch filter must be enforced")
	}
	// Unknown webhook.
	if _, err := store.MatchWebhook(ctx, "whk-missing", sign(body, ts), ts, body, "main"); err == nil {
		t.Fatal("unknown webhook must be rejected")
	}

	// Deliveries are counted for history.
	_, _ = store.MatchWebhook(ctx, webhook.ID, sign(body, ts), ts, body, "main")
	listed, _ = store.ListWebhooks(ctx, deployment.ID)
	if listed[0].Deliveries < 2 {
		t.Fatalf("deliveries = %d", listed[0].Deliveries)
	}

	if err := store.DeleteWebhook(ctx, "default", webhook.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MatchWebhook(ctx, webhook.ID, sign(body, ts), ts, body, "main"); err == nil {
		t.Fatal("deleted webhook must stop accepting deliveries")
	}
}

func TestRevisionRollbackRules(t *testing.T) {
	ctx := context.Background()
	coreStore, store := openDeployStore(t)

	deployment, err := store.CreateDeployment(ctx, "default", "rollback-me", "image", "busybox:latest", "",
		core.PlacementAuto, "", core.Resources{CPUMillis: 250, MemoryBytes: 64 << 20}, nil, "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	// No healthy revision → rollback is refused with a useful error.
	if _, err := store.Rollback(ctx, deployment, "local-admin"); err == nil || !strings.Contains(err.Error(), "healthy") {
		t.Fatalf("rollback without healthy revision = %v", err)
	}
	_ = coreStore
}

func TestDeploymentSyncFromRuntime(t *testing.T) {
	ctx := context.Background()
	coreStore, store := openDeployStore(t)

	deployment, err := store.CreateDeployment(ctx, "default", "stateful-check", "image", "busybox:latest", "",
		core.PlacementAuto, "", core.Resources{CPUMillis: 250, MemoryBytes: 64 << 20}, nil, "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a built revision with a running workload.
	_, _ = coreStore.DB().ExecContext(ctx,
		"INSERT INTO deployment_revisions(id,deployment_id,revision,image,image_digest,source_commit,workload_id,state,note,created_by,created_at) VALUES('rev-1',?,1,'img','digest','','wl-runtime','built','','local-admin',?)",
		deployment.ID, time.Now().UnixNano())
	_, _ = coreStore.DB().ExecContext(ctx, "UPDATE deployments SET current_revision=1 WHERE id=?", deployment.ID)

	spec := core.Submission{Image: "busybox:latest", Kind: "service"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	runtime, _, err := coreStore.Submit(ctx, spec, "deploy-runtime")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = coreStore.DB().ExecContext(ctx, "UPDATE deployment_revisions SET workload_id=? WHERE id='rev-1'", runtime.ID)
	// Runtime is PENDING → deployment reflects 'queued' after sync.
	fresh, err := store.GetDeployment(ctx, "default", deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.SyncDeploymentState(ctx, &fresh)
	if fresh.State != "queued" {
		t.Fatalf("synced state = %s (workload %s)", fresh.State, runtime.State)
	}
}

var _ = fmt.Sprintf
