// Package deploy implements Kamui Deploy: real image builds from authorized
// Git sources or user Dockerfiles, revisioned runtime deployments scheduled
// through the shared workload engine, authenticated webhooks and rollback.
//
// Builds execute on the coordinator's trusted Docker daemon through the Go SDK
// with a bounded, materialized build context — build scripts never see the
// Docker socket, coordinator credentials or arbitrary host paths.
package deploy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

const (
	MaxBuildContextBytes = int64(50) << 20
	MaxBuildLogBytes     = 512 << 10
	BuildTimeout         = 15 * time.Minute
	MaxConcurrentBuilds  = 1
)

var (
	ErrNotFound  = errors.New("deployment not found")
	ErrConflict  = errors.New("conflict")
	ErrBadInput  = errors.New("invalid input")
	ErrForbidden = errors.New("forbidden")

	deployNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	shaRe        = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	gitHosts     = map[string]bool{
		"github.com": true,
	}
)

type Store struct {
	db      *sql.DB
	ids     *identity.Store
	core    *core.Store
	builder *Builder

	routeCheck func(ctx context.Context, workloadID string) (string, error)
	buildSem   chan struct{}
	nowFactory func() time.Time
}

// NewStore wires the deploy store. builder may be nil on hosts without a
// local Docker daemon; builds then fail with a clear status instead of being
// silently skipped.
func NewStore(db *sql.DB, ids *identity.Store, coreStore *core.Store, builder *Builder) *Store {
	return &Store{
		db: db, ids: ids, core: coreStore, builder: builder,
		buildSem:   make(chan struct{}, MaxConcurrentBuilds),
		nowFactory: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Store) now() time.Time { return s.nowFactory() }

func validName(name string) bool { return deployNameRe.MatchString(name) }

// --- Deployment model ---------------------------------------------------------

type Deployment struct {
	ID              string            `json:"id"`
	ProjectID       string            `json:"project_id"`
	Name            string            `json:"name"`
	SourceKind      string            `json:"source_kind"`
	SourceRef       string            `json:"source_ref,omitempty"`
	BuildImage      string            `json:"build_image,omitempty"`
	CurrentRevision int               `json:"current_revision"`
	HealthyRevision int               `json:"healthy_revision"`
	State           string            `json:"state"`
	Placement       string            `json:"placement"`
	WorkerID        string            `json:"worker_id,omitempty"`
	Resources       core.Resources    `json:"resources"`
	Env             map[string]string `json:"env,omitempty"`
	CreatedBy       string            `json:"created_by"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type Revision struct {
	ID           string    `json:"id"`
	DeploymentID string    `json:"deployment_id"`
	Revision     int       `json:"revision"`
	Image        string    `json:"image"`
	ImageDigest  string    `json:"image_digest,omitempty"`
	SourceCommit string    `json:"source_commit,omitempty"`
	WorkloadID   string    `json:"workload_id,omitempty"`
	State        string    `json:"state"`
	Note         string    `json:"note,omitempty"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

type Build struct {
	ID           string     `json:"id"`
	DeploymentID string     `json:"deployment_id"`
	Revision     int        `json:"revision"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason,omitempty"`
	CommitSHA    string     `json:"commit_sha,omitempty"`
	Image        string     `json:"image,omitempty"`
	ImageDigest  string     `json:"image_digest,omitempty"`
	Log          string     `json:"log,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	CreatedBy    string     `json:"created_by"`
}

const deployColumns = "id,project_id,name,source_kind,source_ref,build_image,current_revision,healthy_revision,state,placement,worker_id,resources,env,created_by,created_at,updated_at"

func scanDeployment(row interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	var resources, env []byte
	var createdAt, updatedAt int64
	if err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.SourceKind, &d.SourceRef, &d.BuildImage,
		&d.CurrentRevision, &d.HealthyRevision, &d.State, &d.Placement, &d.WorkerID,
		&resources, &env, &d.CreatedBy, &createdAt, &updatedAt); err != nil {
		return d, err
	}
	_ = json.Unmarshal(resources, &d.Resources)
	_ = json.Unmarshal(env, &d.Env)
	d.CreatedAt = time.Unix(0, createdAt).UTC()
	d.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return d, nil
}

func (s *Store) CreateDeployment(ctx context.Context, projectID, name, sourceKind, sourceRef, buildImage, placement, workerID string, resources core.Resources, env map[string]string, actor string) (Deployment, error) {
	if !validName(name) {
		return Deployment{}, fmt.Errorf("%w: deployment names must be lowercase letters, digits and dashes", ErrBadInput)
	}
	switch sourceKind {
	case "git", "dockerfile", "image":
	default:
		return Deployment{}, fmt.Errorf("%w: source must be git, dockerfile or image", ErrBadInput)
	}
	if sourceKind == "git" {
		if err := ValidateGitSource(sourceRef); err != nil {
			return Deployment{}, err
		}
	}
	if placement == "" {
		placement = core.PlacementAuto
	}
	if placement != core.PlacementAuto && placement != core.PlacementManual && placement != core.PlacementPreferred {
		return Deployment{}, fmt.Errorf("%w: invalid placement mode", ErrBadInput)
	}
	if placement == core.PlacementManual && workerID == "" {
		return Deployment{}, fmt.Errorf("%w: manual placement requires a worker", ErrBadInput)
	}
	if resources.CPUMillis == 0 {
		resources.CPUMillis = 250
	}
	if resources.MemoryBytes == 0 {
		resources.MemoryBytes = 128 << 20
	}
	if err := resources.Validate(); err != nil {
		return Deployment{}, err
	}
	if len(env) > 32 {
		return Deployment{}, fmt.Errorf("%w: too many environment entries", ErrBadInput)
	}
	now := s.now()
	envData, _ := json.Marshal(env)
	resData, _ := json.Marshal(resources)
	id := newID("dep")
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO deployments(id,project_id,name,source_kind,source_ref,build_image,current_revision,healthy_revision,state,placement,worker_id,resources,env,created_by,created_at,updated_at) "+
			"VALUES(?,?,?,?,?,?,'0','0','created',?,?,?,?,?,?,?)",
		id, projectID, name, sourceKind, sourceRef, buildImage, placement, workerID, resData, envData, actor, now.UnixNano(), now.UnixNano())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Deployment{}, fmt.Errorf("%w: a deployment with this name exists in the project", ErrConflict)
		}
		return Deployment{}, err
	}
	return s.GetDeployment(ctx, projectID, id)
}

func (s *Store) GetDeployment(ctx context.Context, projectID, id string) (Deployment, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+deployColumns+" FROM deployments WHERE id=? AND project_id=?", id, projectID)
	deployment, err := scanDeployment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	return deployment, err
}

func (s *Store) FindDeployment(ctx context.Context, projectID, name string) (Deployment, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+deployColumns+" FROM deployments WHERE project_id=? AND name=?", projectID, name)
	deployment, err := scanDeployment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	return deployment, err
}

func (s *Store) GetDeploymentByPath(ctx context.Context, projectID, id string) (Deployment, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+deployColumns+" FROM deployments WHERE id=? AND project_id=?", id, projectID)
	deployment, err := scanDeployment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	return deployment, err
}

func (s *Store) ListDeployments(ctx context.Context, projectID string) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+deployColumns+" FROM deployments WHERE project_id=? ORDER BY created_at DESC LIMIT 100", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Deployment, 0)
	for rows.Next() {
		deployment, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, deployment)
	}
	return items, rows.Err()
}

func (s *Store) DeleteDeployment(ctx context.Context, projectID, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM deployments WHERE id=? AND project_id=?", id, projectID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Revisions and builds -------------------------------------------------------

func scanRevision(row interface{ Scan(...any) error }) (Revision, error) {
	var rev Revision
	var createdAt int64
	if err := row.Scan(&rev.ID, &rev.DeploymentID, &rev.Revision, &rev.Image, &rev.ImageDigest,
		&rev.SourceCommit, &rev.WorkloadID, &rev.State, &rev.Note, &rev.CreatedBy, &createdAt); err != nil {
		return rev, err
	}
	rev.CreatedAt = time.Unix(0, createdAt).UTC()
	return rev, nil
}

const revisionColumns = "id,deployment_id,revision,image,image_digest,source_commit,workload_id,state,note,created_by,created_at"

func (s *Store) ListRevisions(ctx context.Context, deploymentID string) ([]Revision, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+revisionColumns+" FROM deployment_revisions WHERE deployment_id=? ORDER BY revision DESC LIMIT 50", deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Revision, 0)
	for rows.Next() {
		revision, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, revision)
	}
	return items, rows.Err()
}

func (s *Store) GetRevision(ctx context.Context, deploymentID string, revision int) (Revision, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+revisionColumns+" FROM deployment_revisions WHERE deployment_id=? AND revision=?", deploymentID, revision)
	rev, err := scanRevision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	return rev, err
}

func scanBuild(row interface{ Scan(...any) error }) (Build, error) {
	var b Build
	var started, finished sql.NullInt64
	if err := row.Scan(&b.ID, &b.DeploymentID, &b.Revision, &b.Status, &b.Reason, &b.CommitSHA,
		&b.Image, &b.ImageDigest, &b.Log, &started, &finished, &b.CreatedBy); err != nil {
		return b, err
	}
	b.StartedAt = time.Unix(0, started.Int64).UTC()
	if finished.Valid {
		t := time.Unix(0, finished.Int64).UTC()
		b.FinishedAt = &t
	}
	return b, nil
}

const buildColumns = "id,deployment_id,revision,status,reason,commit_sha,image,image_digest,log,started_at,finished_at,created_by"

func (s *Store) ListBuilds(ctx context.Context, deploymentID string, limit int) ([]Build, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+buildColumns+" FROM builds WHERE deployment_id=? ORDER BY started_at DESC LIMIT ?", deploymentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Build, 0)
	for rows.Next() {
		build, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, build)
	}
	return items, rows.Err()
}

// TriggerBuild records a build and executes it on the bounded build queue.
// It returns immediately; inspect the build for progress.
func (s *Store) TriggerBuild(ctx context.Context, deployment Deployment, commit string, gitToken string, dockerfile []byte, actor string) (Build, error) {
	if s.builder == nil {
		return Build{}, fmt.Errorf("%w: this coordinator has no image builder (requires a local Docker daemon)", ErrForbidden)
	}
	var nextRevision int
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision),0)+1 FROM deployment_revisions WHERE deployment_id=?", deployment.ID).Scan(&nextRevision)
	if err != nil {
		return Build{}, err
	}
	now := s.now()
	build := Build{
		ID: strings.ReplaceAll(newID("bld"), "-", ""), DeploymentID: deployment.ID, Revision: nextRevision,
		Status: "queued", CommitSHA: commit, StartedAt: now, CreatedBy: actor,
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO builds(id,deployment_id,revision,status,reason,commit_sha,started_at,created_by) VALUES(?,?,?,?,?,?,?,?)",
		build.ID, deployment.ID, build.Revision, build.Status, "", commit, now.UnixNano(), actor); err != nil {
		return Build{}, err
	}
	// Snapshot the build inputs; the queue worker must not race later edits.
	input := buildInput{
		Build: build, Deployment: deployment, Commit: commit, GitToken: gitToken, Dockerfile: dockerfile,
	}
	select {
	case s.builder.queue <- input:
	default:
		s.finishBuild(ctx, build.ID, "failed", "build queue is full; try again shortly", "", "")
		return s.GetBuild(ctx, build.ID)
	}
	go s.runBuild(input)
	return s.GetBuild(ctx, build.ID)
}

type buildInput struct {
	Build      Build
	Deployment Deployment
	Commit     string
	GitToken   string
	Dockerfile []byte
}

func (s *Store) runBuild(input buildInput) {
	s.buildSem <- struct{}{}
	defer func() { <-s.buildSem }()
	ctx, cancel := context.WithTimeout(context.Background(), BuildTimeout)
	defer cancel()
	_, _ = s.db.ExecContext(ctx, "UPDATE builds SET status='building' WHERE id=?", input.Build.ID)

	var result buildResult
	// The target tag is computed up front so the recorded revision image is
	// the tag actually applied to the built image.
	targetTag := fmt.Sprintf("kamui-deploy/%s:%d", input.Deployment.Name, input.Build.Revision)
	switch input.Deployment.SourceKind {
	case "git":
		result = s.builder.BuildFromGit(ctx, input.Deployment.SourceRef, input.Commit, input.GitToken, targetTag)
	case "dockerfile":
		result = s.builder.BuildFromDockerfile(ctx, input.Dockerfile, targetTag)
	case "image":
		result = s.builder.ResolveImage(ctx, input.Deployment.SourceRef)
	default:
		result = buildResult{Err: fmt.Errorf("unknown source kind")}
	}
	if result.Err != nil {
		s.finishBuild(ctx, input.Build.ID, "failed", result.Err.Error(), "", "")
		_, _ = s.db.ExecContext(ctx, "UPDATE deployments SET state='build_failed', updated_at=? WHERE id=?", s.now().UnixNano(), input.Deployment.ID)
		return
	}
	// Register the revision with the built image.
	next := input.Build.Revision
	imageTag := fmt.Sprintf("kamui-deploy/%s:%d", input.Deployment.Name, next)
	_, _ = s.db.ExecContext(ctx, "UPDATE builds SET status='succeeded', image=?, image_digest=?, finished_at=? WHERE id=?",
		imageTag, result.Digest, s.now().UnixNano(), input.Build.ID)
	revID := strings.ReplaceAll(newID("rev"), "-", "")
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO deployment_revisions(id,deployment_id,revision,image,image_digest,source_commit,workload_id,state,note,created_by,created_at) VALUES(?,?,?,?,?,?,?,'built',?,?,?)",
		revID, input.Deployment.ID, next, imageTag, result.Digest, input.Build.CommitSHA, "", input.Build.CreatedBy, s.now().UnixNano(), s.now().UnixNano())
	if err != nil {
		slog.Error("record deployment revision", "error", err)
		return
	}
	_, _ = s.db.ExecContext(ctx,
		"UPDATE deployments SET current_revision=?, updated_at=? WHERE id=? AND current_revision<?",
		next, s.now().UnixNano(), input.Deployment.ID, next)
}

func (s *Store) finishBuild(ctx context.Context, buildID, status, reason string, image, digest string) {
	_, _ = s.db.ExecContext(ctx,
		"UPDATE builds SET status=?, reason=?, image=?, image_digest=?, finished_at=? WHERE id=?",
		status, reason, image, digest, s.now().UnixNano(), buildID)
}

func (s *Store) GetBuild(ctx context.Context, buildID string) (Build, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+buildColumns+" FROM builds WHERE id=?", buildID)
	build, err := scanBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, ErrNotFound
	}
	return build, err
}

// ReleaseRevision schedules the revision's runtime through the shared engine.
// It never replaces a healthy revision's running workload until the new one
// reaches readiness; rollback pins the previous revision's image.
func (s *Store) ReleaseRevision(ctx context.Context, deployment Deployment, revision int, actor string) (Revision, error) {
	rev, err := s.GetRevision(ctx, deployment.ID, revision)
	if err != nil {
		return Revision{}, err
	}
	submission := core.Submission{
		Service:     "deploy",
		Kind:        "service",
		Image:       rev.Image,
		ImageDigest: rev.ImageDigest,
		Env:         deployment.Env,
		Network:     "private",
		Placement:   deployment.Placement,
		WorkerID:    deployment.WorkerID,
		Priority:    50,
	}
	submission.Resources = deployment.Resources
	submission.SetScope(deployment.ProjectID, actor)
	if err := submission.Normalize(); err != nil {
		return Revision{}, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	workload, _, err := s.core.SubmitForProjectAs(ctx, deployment.ProjectID, actor, submission, "deploy:"+deployment.ID+":"+fmt.Sprint(revision))
	if err != nil {
		if errors.Is(err, core.ErrConflict) {
			// This revision already has its runtime; reuse it.
			_, _ = s.db.ExecContext(ctx, "UPDATE deployments SET state='running', updated_at=? WHERE id=?", s.now().UnixNano(), deployment.ID)
			return s.GetRevision(ctx, deployment.ID, revision)
		}
		return Revision{}, err
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE deployment_revisions SET workload_id=?, state='releasing' WHERE id=?", workload.ID, rev.ID)
	_, _ = s.db.ExecContext(ctx, "UPDATE deployments SET current_revision=?, state='releasing', updated_at=? WHERE id=?", revision, s.now().UnixNano(), deployment.ID)
	return s.GetRevision(ctx, deployment.ID, revision)
}

// SyncDeploymentState derives deployment state from its current revision's
// workload. The engine remains the single writer of workload state.
func (s *Store) SyncDeploymentState(ctx context.Context, deployment *Deployment) {
	if deployment.CurrentRevision == 0 {
		return
	}
	rev, err := s.GetRevision(ctx, deployment.ID, deployment.CurrentRevision)
	if err != nil || rev.WorkloadID == "" {
		return
	}
	workload, err := s.core.Get(ctx, rev.WorkloadID)
	if err != nil {
		return
	}
	updates := map[core.State]string{
		core.Running:   "running",
		core.Starting:  "releasing",
		core.Assigned:  "releasing",
		core.Queued:    "queued",
		core.Pending:   "queued",
		core.Stopped:   "stopped",
		core.Failed:    "failed",
		core.Cancelled: "stopped",
		core.Stopping:  "stopping",
		core.Unhealthy: "unhealthy",
	}
	if state, ok := updates[workload.State]; ok && state != deployment.State {
		_, _ = s.db.ExecContext(ctx, "UPDATE deployments SET state=?, updated_at=? WHERE id=?", state, s.now().UnixNano(), deployment.ID)
		deployment.State = state
		if workload.State == core.Running {
			// Readiness is proven by a live route; a running workload without
			// one is not yet healthy.
			if _, routeErr := s.routeLookup(ctx, workload.ID); routeErr == nil {
				_, _ = s.db.ExecContext(ctx, "UPDATE deployments SET healthy_revision=?, updated_at=? WHERE id=?", deployment.CurrentRevision, s.now().UnixNano(), deployment.ID)
				deployment.HealthyRevision = deployment.CurrentRevision
			}
		}
	}
}

// routeLookup is set by the HTTP layer to avoid a circular dependency on the
// runtime package; it reports whether a live service route exists.
func (s *Store) routeLookup(ctx context.Context, workloadID string) (string, error) {
	if s.routeCheck == nil {
		return "", errors.New("route check unavailable")
	}
	return s.routeCheck(ctx, workloadID)
}

// SetRouteCheck installs the route-lookup hook.
func (s *Store) SetRouteCheck(fn func(ctx context.Context, workloadID string) (string, error)) {
	s.routeCheck = fn
}

// --- Rollback and lifecycle ------------------------------------------------------

func (s *Store) Rollback(ctx context.Context, deployment Deployment, actor string) (Revision, error) {
	if deployment.HealthyRevision <= 0 || deployment.HealthyRevision >= deployment.CurrentRevision {
		return Revision{}, fmt.Errorf("%w: no previous healthy revision to roll back to", ErrConflict)
	}
	rev, err := s.ReleaseRevision(ctx, deployment, deployment.HealthyRevision, actor)
	if err != nil {
		return Revision{}, err
	}
	_, _ = s.db.ExecContext(ctx,
		"UPDATE deployments SET current_revision=?, state='releasing', updated_at=? WHERE id=?",
		deployment.HealthyRevision, s.now().UnixNano(), deployment.ID)
	_, _ = s.db.ExecContext(ctx, "UPDATE deployment_revisions SET note='rollback target' WHERE id=?", rev.ID)
	return s.GetRevision(ctx, deployment.ID, deployment.HealthyRevision)
}

// StopRuntime cancels the current revision's workload.
func (s *Store) StopRuntime(ctx context.Context, deployment Deployment) error {
	if deployment.CurrentRevision == 0 {
		return ErrNotFound
	}
	rev, err := s.GetRevision(ctx, deployment.ID, deployment.CurrentRevision)
	if err != nil || rev.WorkloadID == "" {
		return ErrNotFound
	}
	workload, err := s.core.Get(ctx, rev.WorkloadID)
	if err != nil {
		return err
	}
	if workload.State.Terminal() {
		return nil
	}
	if err := s.cancelWorkload(ctx, rev.WorkloadID); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE deployments SET state='stopping', updated_at=? WHERE id=?", s.now().UnixNano(), deployment.ID)
	return nil
}

// cancelWorkload records cancellation intent through the coordinator's CAS
// path; the engine performs the actual stop.
func (s *Store) cancelWorkload(ctx context.Context, workloadID string) error {
	for {
		workload, err := s.core.Get(ctx, workloadID)
		if err != nil || workload.State.Terminal() || workload.CancelRequested {
			return err
		}
		workload.CancelRequested = true
		if err := s.core.Save(ctx, workload, false); !errors.Is(err, core.ErrConflict) {
			return err
		}
	}
}

func (s *Store) StartRuntime(ctx context.Context, deployment Deployment, actor string) error {
	if deployment.CurrentRevision == 0 {
		return fmt.Errorf("%w: nothing has been built yet", ErrConflict)
	}
	_, err := s.ReleaseRevision(ctx, deployment, deployment.CurrentRevision, actor)
	return err
}

// --- Webhooks ---------------------------------------------------------------------

type Webhook struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	DeploymentID string    `json:"deployment_id"`
	Secret       string    `json:"-"`
	BranchFilter string    `json:"branch_filter,omitempty"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	Deliveries   int       `json:"deliveries"`
}

func (s *Store) CreateWebhook(ctx context.Context, projectID, deploymentID, branchFilter, actor string) (Webhook, string, error) {
	secret, err := identity.NewSecretToken("whsec", 24)
	if err != nil {
		return Webhook{}, "", err
	}
	id := strings.ReplaceAll(newID("whk"), "-", "")
	now := s.now().UnixNano()
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO deploy_webhooks(id,project_id,deployment_id,secret,branch_filter,enabled,created_at) VALUES(?,?,?,?,?,1,?)",
		id, projectID, deploymentID, secret, branchFilter, now)
	if err != nil {
		return Webhook{}, "", err
	}
	return Webhook{ID: id, ProjectID: projectID, DeploymentID: deploymentID, Secret: secret,
		BranchFilter: branchFilter, Enabled: true, CreatedAt: time.Unix(0, now).UTC()}, secret, nil
}

func (s *Store) ListWebhooks(ctx context.Context, deploymentID string) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id,project_id,deployment_id,branch_filter,enabled,created_at,deliveries FROM deploy_webhooks WHERE deployment_id=? ORDER BY created_at DESC", deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Webhook, 0)
	for rows.Next() {
		var w Webhook
		var enabled, createdAt int
		if err := rows.Scan(&w.ID, &w.ProjectID, &w.DeploymentID, &w.BranchFilter, &enabled, &createdAt, &w.Deliveries); err != nil {
			return nil, err
		}
		w.Enabled = enabled == 1
		w.CreatedAt = time.Unix(0, int64(createdAt)).UTC()
		items = append(items, w)
	}
	return items, rows.Err()
}

func (s *Store) DeleteWebhook(ctx context.Context, projectID, webhookID string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM deploy_webhooks WHERE id=? AND project_id=?", webhookID, projectID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

type WebhookTrigger struct {
	Deployment Deployment
	Commit     string
	Branch     string
	Actor      string
}

// MatchWebhook authenticates a signed delivery. Signature is HMAC-SHA256 over
// "timestamp.body" with the webhook secret; deliveries older than five minutes
// are rejected to bound replay.
func (s *Store) MatchWebhook(ctx context.Context, webhookID, signature, timestamp, body string, payloadBranch string) (WebhookTrigger, error) {
	var w Webhook
	var enabled, createdAt int
	err := s.db.QueryRowContext(ctx,
		"SELECT id,project_id,deployment_id,secret,branch_filter,enabled,created_at,deliveries FROM deploy_webhooks WHERE id=? AND enabled=1", webhookID).
		Scan(&w.ID, &w.ProjectID, &w.DeploymentID, &w.Secret, &w.BranchFilter, &enabled, &createdAt, &w.Deliveries)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookTrigger{}, ErrNotFound
	}
	if err != nil {
		return WebhookTrigger{}, err
	}
	w.CreatedAt = time.Unix(0, int64(createdAt)).UTC()
	stamp, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(stamp, 0)) > 5*time.Minute || time.Unix(stamp, 0).After(time.Now().Add(time.Minute)) {
		return WebhookTrigger{}, fmt.Errorf("%w: stale or invalid timestamp", ErrForbidden)
	}
	mac := hmac.New(sha256.New, []byte(w.Secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte(body))
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return WebhookTrigger{}, fmt.Errorf("%w: signature mismatch", ErrForbidden)
	}
	if w.BranchFilter != "" && payloadBranch != w.BranchFilter {
		return WebhookTrigger{}, fmt.Errorf("%w: branch %s does not match the trigger filter", ErrForbidden, payloadBranch)
	}
	deployment, err := s.GetDeploymentByID(ctx, w.DeploymentID)
	if err != nil {
		return WebhookTrigger{}, err
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE deploy_webhooks SET deliveries=deliveries+1 WHERE id=?", w.ID)
	return WebhookTrigger{Deployment: deployment, Commit: "", Branch: payloadBranch, Actor: "webhook:" + w.ID}, nil
}

func (s *Store) GetDeploymentByID(ctx context.Context, id string) (Deployment, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+deployColumns+" FROM deployments WHERE id=?", id)
	deployment, err := scanDeployment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	return deployment, err
}

// --- Source validation -------------------------------------------------------------

// ValidateGitSource accepts only explicit https GitHub URLs of trusted hosts.
func ValidateGitSource(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("%w: repository source must be an https URL", ErrBadInput)
	}
	if !gitHosts[parsed.Hostname()] {
		return fmt.Errorf("%w: repository host %s is not an authorized source", ErrBadInput, parsed.Hostname())
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%w: repository URL must not carry credentials, query or fragment", ErrBadInput)
	}
	return nil
}

func ValidCommit(sha string) bool { return sha == "" || shaRe.MatchString(sha) }
