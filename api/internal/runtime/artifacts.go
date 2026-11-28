// Package runtime implements the shared distributed-execution prerequisites:
// project artifact staging, secret resolution, scoped registry credentials,
// persistent volume placement, controlled network profiles and assignment-bound
// service routes.
package runtime

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

const (
	artifactPrefix = "art"
	// MaxArtifactBytes bounds a single artifact blob (default 1 GiB).
	MaxArtifactBytes = int64(1) << 30
	// MaxObjectName bounds stored object keys and artifact names.
	MaxObjectName = 512
)

var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("already exists")
	ErrUnauthorized = errors.New("unauthorized")
	ErrBadInput     = errors.New("invalid input")
	ErrTooLarge     = errors.New("payload exceeds the configured limit")

	nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
)

// Store is the shared runtime metadata store. It shares the coordinator's
// SQLite database and never stores secret material in plaintext.
type Store struct {
	db      *sql.DB
	master  string
	root    string
	maxBlob int64
}

// NewStore opens the runtime store over the coordinator database. root is the
// storage root for artifact and backup blobs.
func NewStore(db *sql.DB, masterSecret, root string) *Store {
	return &Store{db: db, master: masterSecret, root: root, maxBlob: MaxArtifactBytes}
}

func (s *Store) DB() *sql.DB { return s.db }

// Root returns the configured storage root.
func (s *Store) Root() string { return s.root }

// MaxBlobBytes is the configured per-blob size limit.
func (s *Store) MaxBlobBytes() int64 { return s.maxBlob }

func (s *Store) now() time.Time { return time.Now().UTC() }

func newID(prefix string) string {
	var bytes [16]byte
	_, _ = rand.Read(bytes[:])
	return prefix + "-" + hex.EncodeToString(bytes[:])
}

func validName(name string) bool { return nameRe.MatchString(name) }

// --- Artifacts -------------------------------------------------------------

type Artifact struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"project_id"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	SizeBytes    int64      `json:"size_bytes"`
	Checksum     string     `json:"checksum"`
	ContentType  string     `json:"content_type"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	LastDownload *time.Time `json:"last_download_at,omitempty"`
}

func scanArtifact(row interface{ Scan(...any) error }) (Artifact, error) {
	var a Artifact
	var createdAt, lastDownload sql.NullInt64
	if err := row.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Kind, &a.SizeBytes, &a.Checksum,
		&a.ContentType, &a.CreatedBy, &createdAt, &lastDownload); err != nil {
		return a, err
	}
	a.CreatedAt = time.Unix(0, createdAt.Int64).UTC()
	if lastDownload.Valid {
		t := time.Unix(0, lastDownload.Int64).UTC()
		a.LastDownload = &t
	}
	return a, nil
}

const artifactColumns = "id,project_id,name,kind,size_bytes,checksum,content_type,created_by,created_at,last_download_at"

func (s *Store) blobPath(projectID, id string) (string, error) {
	if !validName(projectID) && projectID != "" {
		return "", ErrBadInput
	}
	dir := filepath.Join(s.root, "artifacts", projectID, id)
	// The path is assembled from validated components; clean it anyway so a
	// traversal attempt can never escape the storage root.
	if !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(s.root)+string(os.PathSeparator)) {
		return "", ErrBadInput
	}
	return dir, nil
}

// CreateArtifact stores a streamed upload atomically. The blob is written to a
// temporary file, checksummed while streaming, then renamed into place so a
// failed or truncated upload never becomes visible.
func (s *Store) CreateArtifact(ctx context.Context, projectID, name, kind, contentType, createdBy string, body io.Reader) (Artifact, error) {
	if !validName(name) || !validName(kind) {
		return Artifact{}, fmt.Errorf("%w: artifact name and kind are invalid", ErrBadInput)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if len(contentType) > 128 || strings.ContainsAny(contentType, "\r\n") {
		return Artifact{}, fmt.Errorf("%w: invalid content type", ErrBadInput)
	}
	id := newID(artifactPrefix)
	dir, err := s.blobPath(projectID, id)
	if err != nil {
		return Artifact{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Artifact{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dir), ".upload-*")
	if err != nil {
		return Artifact{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(body, s.maxBlob+1))
	closeErr := tmp.Close()
	if err != nil {
		return Artifact{}, err
	}
	if closeErr != nil {
		return Artifact{}, closeErr
	}
	if size > s.maxBlob {
		return Artifact{}, ErrTooLarge
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Artifact{}, err
	}
	defer tx.Rollback()
	checksum := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	artifact := Artifact{
		ID: id, ProjectID: projectID, Name: name, Kind: kind, SizeBytes: size,
		Checksum: checksum, ContentType: contentType, CreatedBy: createdBy, CreatedAt: now,
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO artifacts(id,project_id,name,kind,size_bytes,checksum,content_type,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
		artifact.ID, projectID, name, kind, size, checksum, contentType, createdBy, now.UnixNano())
	if err != nil {
		if isUniqueViolation(err) {
			return Artifact{}, fmt.Errorf("%w: artifact name already exists in this project", ErrConflict)
		}
		return Artifact{}, err
	}
	if err := tx.Commit(); err != nil {
		return Artifact{}, err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, "blob")); err != nil {
		_, _ = s.db.ExecContext(ctx, "DELETE FROM artifacts WHERE id=?", id)
		return Artifact{}, err
	}
	return artifact, nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

func (s *Store) GetArtifact(ctx context.Context, projectID, id string) (Artifact, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+artifactColumns+" FROM artifacts WHERE id=? AND project_id=?", id, projectID)
	artifact, err := scanArtifact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	return artifact, err
}

func (s *Store) ListArtifacts(ctx context.Context, projectID string, limit int) ([]Artifact, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+artifactColumns+" FROM artifacts WHERE project_id=? ORDER BY created_at DESC LIMIT ?", projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Artifact, 0)
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, artifact)
	}
	return items, rows.Err()
}

// OpenArtifact opens a regular blob whose size matches its stored metadata.
func (s *Store) OpenArtifact(ctx context.Context, artifact Artifact) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	dir, err := s.blobPath(artifact.ProjectID, artifact.ID)
	if err != nil {
		return nil, 0, err
	}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()
	info, err := root.Lstat("blob")
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() != artifact.SizeBytes || info.Size() > s.maxBlob {
		return nil, 0, fmt.Errorf("%w: artifact blob has an invalid type or size", ErrBadInput)
	}
	file, err := root.Open("blob")
	if err != nil {
		return nil, 0, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		file.Close()
		return nil, 0, fmt.Errorf("%w: artifact blob changed", ErrBadInput)
	}
	return file, info.Size(), nil
}

func (s *Store) MarkArtifactDownloaded(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx, "UPDATE artifacts SET last_download_at=? WHERE id=?", s.now().UnixNano(), id)
}

func (s *Store) DeleteArtifact(ctx context.Context, projectID, id string) error {
	dir, err := s.blobPath(projectID, id)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM artifacts WHERE id=? AND project_id=?", id, projectID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return os.RemoveAll(dir)
}

// VerifyChecksum streams a blob and compares it to the recorded digest.
func (s *Store) VerifyChecksum(ctx context.Context, artifact Artifact) error {
	reader, size, err := s.OpenArtifact(ctx, artifact)
	if err != nil {
		return err
	}
	defer reader.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(artifactContextReader{ctx, reader}, size+1))
	if err != nil {
		return err
	}
	sum := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if n != size || !hmac.Equal([]byte(sum), []byte(artifact.Checksum)) {
		return fmt.Errorf("%w: artifact integrity verification failed", ErrBadInput)
	}
	return nil
}

type artifactContextReader struct {
	ctx context.Context
	io.Reader
}

func (r artifactContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// --- Registry credentials ----------------------------------------------------

type RegistryCredential struct {
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	Password  string    `json:"-"`
	UpdatedAt time.Time `json:"updated_at"`
}

func registryKey(master, host, projectID string) [32]byte {
	return identity.DeriveEncryptionKey(master + "\x00registry:" + host + "\x00" + projectID)
}

// PutRegistryCredential stores scoped registry credentials sealed with a
// per-host, per-project key.
func (s *Store) PutRegistryCredential(ctx context.Context, projectID, host, username, password, actor string) error {
	if !validRegistryHost(host) || username == "" || password == "" || len(password) > 4096 {
		return fmt.Errorf("%w: invalid registry credential", ErrBadInput)
	}
	key := registryKey(s.master, host, projectID)
	sealedUser, err := identity.Seal(key, username)
	if err != nil {
		return err
	}
	sealedPass, err := identity.Seal(key, password)
	if err != nil {
		return err
	}
	now := s.now().UnixNano()
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO registry_credentials(host,project_id,sealed_username,sealed_password,created_at,updated_at) VALUES(?,?,?,?,?,?) "+
			"ON CONFLICT(host,project_id) DO UPDATE SET sealed_username=excluded.sealed_username, sealed_password=excluded.sealed_password, updated_at=excluded.updated_at",
		host, projectID, sealedUser, sealedPass, now, now)
	return err
}

func (s *Store) DeleteRegistryCredential(ctx context.Context, projectID, host string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM registry_credentials WHERE host=? AND project_id=?", host, projectID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func validRegistryHost(host string) bool {
	return len(host) > 0 && len(host) < 256 &&
		!strings.ContainsAny(host, "/ \r\n\t@") &&
		strings.ContainsAny(host, ".:")
}

// registryAuthFor returns the scoped credential for one assignment's image
// host, or nil when the project has none for it.
func (s *Store) registryAuthFor(ctx context.Context, projectID, host string) (username, password string, err error) {
	key := registryKey(s.master, host, projectID)
	var sealedUser, sealedPass string
	err = s.db.QueryRowContext(ctx,
		"SELECT sealed_username,sealed_password FROM registry_credentials WHERE host=? AND project_id=?", host, projectID).
		Scan(&sealedUser, &sealedPass)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	username, err = identity.Open(key, sealedUser)
	if err != nil {
		return "", "", err
	}
	password, err = identity.Open(key, sealedPass)
	if err != nil {
		return "", "", err
	}
	return username, password, nil
}

// --- Secrets -----------------------------------------------------------------

func secretKey(master, projectID, name string) [32]byte {
	return identity.DeriveEncryptionKey(master + "\x00secret:" + projectID + "\x00" + name)
}

type SecretMeta struct {
	ProjectID string     `json:"project_id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at,omitempty"`
}

// PutSecret stores a sealed project secret. There is deliberately no read of
// the value through the API; it exists for assignment-scoped injection.
func (s *Store) PutSecret(ctx context.Context, projectID, name, value, actor string) error {
	if !validSecretName(name) || value == "" || len(value) > 65536 {
		return fmt.Errorf("%w: invalid secret name or value", ErrBadInput)
	}
	key := secretKey(s.master, projectID, name)
	sealed, err := identity.Seal(key, value)
	if err != nil {
		return err
	}
	now := s.now().UnixNano()
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO secrets(project_id,name,sealed_value,created_by,created_at) VALUES(?,?,?,?,?) "+
			"ON CONFLICT(project_id,name) DO UPDATE SET sealed_value=excluded.sealed_value, rotated_at=excluded.created_at",
		projectID, name, sealed, actor, now)
	return err
}

func (s *Store) DeleteSecret(ctx context.Context, projectID, name string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM secrets WHERE project_id=? AND name=?", projectID, name)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListSecrets(ctx context.Context, projectID string) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT project_id,name,created_at,rotated_at FROM secrets WHERE project_id=? ORDER BY name", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]SecretMeta, 0)
	for rows.Next() {
		var meta SecretMeta
		var createdAt, rotated sql.NullInt64
		if err := rows.Scan(&meta.ProjectID, &meta.Name, &createdAt, &rotated); err != nil {
			return nil, err
		}
		meta.CreatedAt = time.Unix(0, createdAt.Int64).UTC()
		if rotated.Valid {
			t := time.Unix(0, rotated.Int64).UTC()
			meta.RotatedAt = &t
		}
		items = append(items, meta)
	}
	return items, rows.Err()
}

func validSecretName(name string) bool { return nameRe.MatchString(name) }

// ResolveEnv expands `secret:NAME` references into actual values for exactly
// one assignment. The resolved map never leaves the assignment payload.
func (s *Store) ResolveEnv(ctx context.Context, projectID string, env map[string]string) (map[string]string, []string, error) {
	if len(env) == 0 {
		return nil, nil, nil
	}
	resolved := make(map[string]string, len(env))
	var missing []string
	for key, value := range env {
		name, ok := strings.CutPrefix(value, "secret:")
		if !ok {
			resolved[key] = value
			continue
		}
		var sealed string
		err := s.db.QueryRowContext(ctx,
			"SELECT sealed_value FROM secrets WHERE project_id=? AND name=?", projectID, name).Scan(&sealed)
		if errors.Is(err, sql.ErrNoRows) {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		plain, err := identity.Open(secretKey(s.master, projectID, name), sealed)
		if err != nil {
			return nil, nil, err
		}
		resolved[key] = plain
	}
	if len(missing) > 0 {
		return nil, missing, fmt.Errorf("%w: unknown secrets: %s", ErrBadInput, strings.Join(missing, ", "))
	}
	return resolved, nil, nil
}

// --- Persistent volume placement ----------------------------------------------

type VolumePlacement struct {
	ProjectID  string     `json:"project_id"`
	Name       string     `json:"name"`
	WorkerID   string     `json:"worker_id"`
	SizeBytes  int64      `json:"size_bytes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// VolumeOwner returns the worker that owns a persistent volume, or "" when the
// volume has never been placed.
func (s *Store) VolumeOwner(ctx context.Context, projectID, name string) (string, error) {
	var workerID string
	err := s.db.QueryRowContext(ctx,
		"SELECT worker_id FROM volume_placements WHERE project_id=? AND name=?", projectID, name).Scan(&workerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return workerID, err
}

// RecordVolumePlacement pins a volume to a worker on first use and refreshes
// last_used on subsequent assignments.
func (s *Store) RecordVolumePlacement(ctx context.Context, projectID, name, workerID string, sizeBytes int64) error {
	if name == "" || workerID == "" {
		return fmt.Errorf("%w: volume placement requires a name and worker", ErrBadInput)
	}
	now := s.now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO volume_placements(project_id,name,worker_id,size_bytes,created_at,last_used_at) VALUES(?,?,?,?,?,?) "+
			"ON CONFLICT(project_id,name) DO UPDATE SET last_used_at=excluded.last_used_at, size_bytes=MAX(COALESCE(size_bytes,0), excluded.size_bytes)",
		projectID, name, workerID, sizeBytes, now, now)
	return err
}

// ListVolumePlacements returns placement records for a project.
func (s *Store) ListVolumePlacements(ctx context.Context, projectID string) ([]VolumePlacement, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT project_id,name,worker_id,size_bytes,created_at,last_used_at FROM volume_placements WHERE project_id=? ORDER BY name", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]VolumePlacement, 0)
	for rows.Next() {
		var placement VolumePlacement
		var createdAt, lastUsed sql.NullInt64
		if err := rows.Scan(&placement.ProjectID, &placement.Name, &placement.WorkerID, &placement.SizeBytes, &createdAt, &lastUsed); err != nil {
			return nil, err
		}
		placement.CreatedAt = time.Unix(0, createdAt.Int64).UTC()
		if lastUsed.Valid {
			t := time.Unix(0, lastUsed.Int64).UTC()
			placement.LastUsedAt = &t
		}
		items = append(items, placement)
	}
	return items, rows.Err()
}

// --- Service routes ------------------------------------------------------------

type ServiceRoute struct {
	WorkloadID   string    `json:"workload_id"`
	ProjectID    string    `json:"project_id"`
	WorkerID     string    `json:"worker_id"`
	Attempt      int       `json:"attempt"`
	Fence        string    `json:"-"`
	Port         int       `json:"port"`
	Kind         string    `json:"kind"`
	RegisteredAt time.Time `json:"registered_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// RegisterRoute records a service route bound to the current assignment. The
// caller must have verified worker identity and assignment fencing; a stale
// assignment can never register or replace a route.
func (s *Store) RegisterRoute(ctx context.Context, workloadID, projectID, workerID string, attempt int, fence string, port int, kind string) error {
	if port < 1 || port > 65535 || (kind != "http" && kind != "tcp") {
		return fmt.Errorf("%w: invalid service route", ErrBadInput)
	}
	now := s.now()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO service_routes(workload_id,project_id,worker_id,attempt,fence,port,kind,registered_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?) "+
			"ON CONFLICT(workload_id) DO UPDATE SET worker_id=excluded.worker_id, attempt=excluded.attempt, fence=excluded.fence, port=excluded.port, kind=excluded.kind, registered_at=excluded.registered_at, expires_at=excluded.expires_at",
		workloadID, projectID, workerID, attempt, fence, port, kind, now.UnixNano(), now.Add(2*core.OfflineAfter).UnixNano())
	return err
}

func (s *Store) RevokeRoute(ctx context.Context, workloadID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM service_routes WHERE workload_id=?", workloadID)
	return err
}

func (s *Store) GetRoute(ctx context.Context, workloadID string) (ServiceRoute, error) {
	var route ServiceRoute
	var registered, expires int64
	err := s.db.QueryRowContext(ctx,
		"SELECT workload_id,project_id,worker_id,attempt,fence,port,kind,registered_at,expires_at FROM service_routes WHERE workload_id=?", workloadID).
		Scan(&route.WorkloadID, &route.ProjectID, &route.WorkerID, &route.Attempt, &route.Fence, &route.Port, &route.Kind, &registered, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceRoute{}, ErrNotFound
	}
	if err != nil {
		return ServiceRoute{}, err
	}
	route.RegisteredAt = time.Unix(0, registered).UTC()
	route.ExpiresAt = time.Unix(0, expires).UTC()
	return route, nil
}

// PruneRoutes drops expired routes; the engine calls it periodically.
func (s *Store) PruneRoutes(ctx context.Context) {
	_, _ = s.db.ExecContext(ctx, "DELETE FROM service_routes WHERE expires_at<?", s.now().UnixNano())
}

// RouteLookup resolves the live route for a workload, for the service proxy.
func (s *Store) RouteLookup(ctx context.Context, workloadID string) (ServiceRoute, error) {
	route, err := s.GetRoute(ctx, workloadID)
	if err == nil && !route.ExpiresAt.After(s.now()) {
		return ServiceRoute{}, ErrNotFound
	}
	return route, err
}

// --- Assignment payload assembly -------------------------------------------------

// AssignmentEnvelope is the coordinator-resolved, assignment-scoped payload a
// worker receives for one assignment. Secrets are resolved here and never
// logged; registry credentials are scoped to the assignment's image host.
// Problems carries non-fatal resolution failures; a worker must refuse to
// start an assignment whose envelope lists problems.
type AssignmentEnvelope struct {
	Problems     []string          `json:"problems,omitempty"`
	Image        string            `json:"image"`
	ImageDigest  string            `json:"image_digest,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Network      string            `json:"network,omitempty"`
	ProjectNet   string            `json:"project_network,omitempty"`
	Volumes      []VolumeRequest   `json:"volumes,omitempty"`
	Artifacts    []ArtifactMount   `json:"artifacts,omitempty"`
	Outputs      []string          `json:"outputs,omitempty"`
	HealthCheck  *core.HealthCheck `json:"health_check,omitempty"`
	RegistryAuth *RegistryAuth     `json:"registry_auth,omitempty"`
}

type VolumeRequest struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

type ArtifactMount struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path"`
}

type RegistryAuth struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// BuildEnvelope resolves everything a worker needs to execute one assignment.
// It fails closed: unknown secrets or unauthorized artifacts abort the
// assignment rather than degrading it.
func (s *Store) BuildEnvelope(ctx context.Context, projectID string, submission core.Submission) (AssignmentEnvelope, []string, error) {
	envelope := AssignmentEnvelope{
		Image:       submission.Image,
		ImageDigest: submission.ImageDigest,
		Args:        submission.Args,
		Network:     submission.Network,
		Volumes:     make([]VolumeRequest, 0, len(submission.Volumes)),
		Artifacts:   make([]ArtifactMount, 0, len(submission.Artifacts)),
		Outputs:     submission.Outputs,
		HealthCheck: submission.HealthCheck,
	}
	var problems []string
	env, missing, err := s.ResolveEnv(ctx, projectID, submission.Env)
	if err != nil {
		if len(missing) > 0 {
			problems = append(problems, "unknown secrets: "+strings.Join(missing, ", "))
		} else {
			return envelope, problems, err
		}
	}
	envelope.Env = env
	if !submission.NetworkIsolated() && submission.Network == "private" {
		envelope.ProjectNet = ProjectNetworkName(projectID)
	}
	for _, volume := range submission.Volumes {
		envelope.Volumes = append(envelope.Volumes, VolumeRequest{Name: volume.Name, SizeBytes: volume.SizeBytes})
	}
	for _, mount := range submission.Artifacts {
		artifact, err := s.GetArtifact(ctx, projectID, mount.ArtifactID)
		if errors.Is(err, ErrNotFound) {
			problems = append(problems, "unknown artifact "+mount.ArtifactID)
			continue
		}
		if err != nil {
			return envelope, problems, err
		}
		if err := s.VerifyChecksum(ctx, artifact); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		envelope.Artifacts = append(envelope.Artifacts, ArtifactMount{ArtifactID: artifact.ID, Path: mount.Path})
	}
	if host := core.RegistryHost(submission.Image); host != "docker.io" {
		username, password, err := s.registryAuthFor(ctx, projectID, host)
		if err != nil {
			return envelope, problems, err
		}
		if username != "" {
			envelope.RegistryAuth = &RegistryAuth{Host: host, Username: username, Password: password}
		}
	}
	return envelope, problems, nil
}

// ProjectNetworkName delegates to the core derivation so every executor and
// the scheduler agree on the private network name.
func ProjectNetworkName(projectID string) string {
	return core.ProjectNetworkName(projectID)
}

// MarshalEnvelope serializes the envelope for the durable report queue path.
func MarshalEnvelope(envelope AssignmentEnvelope) ([]byte, error) {
	return json.Marshal(envelope)
}
