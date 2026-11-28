// Package ml implements Kamui Train and Kamui Infer on the shared scheduler:
// training jobs execute as real workloads with dataset artifacts and output
// collection, metrics come only from the training process, and inference
// services are OpenAI-compatible proxies over replaceable runtimes.
package ml

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrBadInput = errors.New("invalid input")

	nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
)

type TrainStore struct {
	db   *sql.DB
	core *core.Store
}

func NewTrainStore(db *sql.DB, coreStore *core.Store) *TrainStore {
	return &TrainStore{db: db, core: coreStore}
}

type TrainingJob struct {
	ID                 string    `json:"id"`
	ProjectID          string    `json:"project_id"`
	Name               string    `json:"name"`
	WorkloadID         string    `json:"workload_id,omitempty"`
	DatasetArtifact    string    `json:"dataset_artifact,omitempty"`
	ModelArtifact      string    `json:"model_artifact,omitempty"`
	State              string    `json:"state"`
	Reason             string    `json:"reason,omitempty"`
	CreatedBy          string    `json:"created_by"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	CheckpointArtifact string    `json:"checkpoint_artifact,omitempty"`
	CheckpointStep     int       `json:"checkpoint_step,omitempty"`
	ResumeFrom         string    `json:"resume_from,omitempty"`
	CheckpointPath     string    `json:"checkpoint_path,omitempty"`
}

const trainingColumns = "id,project_id,name,workload_id,dataset_artifact,model_artifact,state,reason,created_by,created_at,updated_at,checkpoint_artifact,checkpoint_step,resume_from,checkpoint_path"

func scanTraining(row interface{ Scan(...any) error }) (TrainingJob, error) {
	var j TrainingJob
	var createdAt, updatedAt int64
	var checkpointArtifact, checkpointPath, resumeFrom string
	var checkpointStep int64
	if err := row.Scan(&j.ID, &j.ProjectID, &j.Name, &j.WorkloadID, &j.DatasetArtifact, &j.ModelArtifact,
		&j.State, &j.Reason, &j.CreatedBy, &createdAt, &updatedAt, &checkpointArtifact, &checkpointStep, &resumeFrom, &checkpointPath); err != nil {
		return j, err
	}
	j.CreatedAt = time.Unix(0, createdAt).UTC()
	j.UpdatedAt = time.Unix(0, updatedAt).UTC()
	j.CheckpointArtifact = checkpointArtifact
	j.CheckpointStep = int(checkpointStep)
	j.ResumeFrom = resumeFrom
	j.CheckpointPath = checkpointPath
	return j, nil
}

// SubmitTraining creates the training job and schedules its workload through
// the shared engine. GPU requests flow through the normal eligibility filter,
// so hosts without compatible GPUs queue the workload with an explanation.
func (s *TrainStore) SubmitTraining(ctx context.Context, projectID, name, image string, args []string, imageDigest string, resources core.Resources, datasetArtifact string, modelArtifact string, resumeFrom string, placement, workerID string, env map[string]string, timeoutSeconds, maxRetries int, actor string) (TrainingJob, error) {
	if !nameRe.MatchString(name) {
		return TrainingJob{}, fmt.Errorf("%w: training names must be lowercase letters, digits and dashes", ErrBadInput)
	}
	var datasetMount []core.ArtifactMount
	if datasetArtifact != "" {
		if !strings.HasPrefix(datasetArtifact, "art-") {
			return TrainingJob{}, fmt.Errorf("%w: dataset_artifact must reference a project artifact", ErrBadInput)
		}
		datasetMount = append(datasetMount, core.ArtifactMount{ArtifactID: datasetArtifact, Path: "/kamui/dataset"})
	}
	submission := core.Submission{
		Service:     "train",
		Kind:        "job",
		Image:       image,
		ImageDigest: imageDigest,
		Args:        args,
		Env:         env,
		Resources:   resources,
		Artifacts:   datasetMount,
		Outputs:     []string{"/out/model", "/out/checkpoints", "/out/metrics.jsonl"},
		Placement:   placement,
		WorkerID:    workerID,
	}
	if timeoutSeconds > 0 {
		submission.Policy.TimeoutSeconds = timeoutSeconds
	}
	if maxRetries >= 0 {
		submission.Policy.MaxRetries = maxRetries
	}
	submission.SetScope(projectID, actor)
	if err := submission.Normalize(); err != nil {
		return TrainingJob{}, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	workload, _, err := s.core.SubmitForProjectAs(ctx, projectID, actor, submission, "train:"+name+":"+fmt.Sprint(time.Now().UnixNano()))
	if err != nil {
		if errors.Is(err, core.ErrConflict) {
			return TrainingJob{}, fmt.Errorf("%w: an identical training submission is already running", ErrConflict)
		}
		return TrainingJob{}, err
	}
	now := time.Now().UTC().UnixNano()
	id := "trn-" + strings.TrimPrefix(workload.ID, "wl-")
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO training_jobs(id,project_id,name,workload_id,dataset_artifact,model_artifact,resume_from,state,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?, 'running',?,?,?)",
		id, projectID, name, workload.ID, datasetArtifact, modelArtifact, resumeFrom, actor, now, now); err != nil {
		return TrainingJob{}, err
	}
	return s.GetTraining(ctx, projectID, id)
}

func (s *TrainStore) GetTraining(ctx context.Context, projectID, id string) (TrainingJob, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+trainingColumns+" FROM training_jobs WHERE id=? AND project_id=?", id, projectID)
	job, err := scanTraining(row)
	if errors.Is(err, sql.ErrNoRows) {
		return TrainingJob{}, ErrNotFound
	}
	return job, err
}

func (s *TrainStore) ListTrainings(ctx context.Context, projectID string) ([]TrainingJob, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+trainingColumns+" FROM training_jobs WHERE project_id=? ORDER BY created_at DESC LIMIT 100", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TrainingJob, 0)
	for rows.Next() {
		job, err := scanTraining(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, job)
	}
	return items, rows.Err()
}

// SyncTrainingState derives the job state from its runtime workload.
func (s *TrainStore) SyncTrainingState(ctx context.Context, job *TrainingJob) {
	if job.WorkloadID == "" {
		return
	}
	workload, err := s.core.Get(ctx, job.WorkloadID)
	if err != nil {
		return
	}
	states := map[core.State]string{
		core.Running: "running", core.Starting: "running", core.Assigned: "queued",
		core.Queued: "queued", core.Pending: "queued", core.Succeeded: "succeeded",
		core.Failed: "failed", core.Cancelled: "cancelled", core.Stopped: "cancelled",
		core.Stopping: "running", core.Unhealthy: "running",
	}
	state := states[workload.State]
	reason := workload.StopReason
	if workload.Result != nil && workload.Result.Reason != "" {
		reason = workload.Result.Reason
	}
	if state != job.State || reason != job.Reason {
		_, _ = s.db.ExecContext(ctx, "UPDATE training_jobs SET state=?, reason=?, updated_at=? WHERE id=?",
			state, reason, time.Now().UTC().UnixNano(), job.ID)
		job.State, job.Reason = state, reason
	}
}

// IngestMetrics records metric lines observed from the training process.
// The parser accepts JSON lines like {"step": 10, "loss": 0.42, "metric": "loss"};
// values are never synthesized.
func (s *TrainStore) IngestMetrics(ctx context.Context, trainingID, blobPath string) (int, error) {
	file, err := os.Open(blobPath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	now := time.Now().UTC().UnixNano()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var entry struct {
			Step     int      `json:"step"`
			Loss     *float64 `json:"loss"`
			Accuracy *float64 `json:"accuracy"`
			Metric   string   `json:"metric"`
			Value    *float64 `json:"value"`
			StepAlt  *int     `json:"epoch"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		pairs := []struct {
			name  string
			value *float64
		}{
			{entry.Metric, entry.Value},
			{"loss", entry.Loss},
			{"accuracy", entry.Accuracy},
		}
		for _, pair := range pairs {
			if pair.value == nil || pair.name == "" {
				continue
			}
			step := entry.Step
			if entry.StepAlt != nil && step == 0 {
				step = *entry.StepAlt
			}
			if _, err := s.db.ExecContext(ctx,
				"INSERT INTO training_metrics(training_id,step,metric,value,at) VALUES(?,?,?,?,?)",
				trainingID, step, pair.name, *pair.value, now); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, scanner.Err()
}

func (s *TrainStore) ListMetrics(ctx context.Context, trainingID string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT step,metric,value,at FROM training_metrics WHERE training_id=? ORDER BY id DESC LIMIT ?", trainingID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var step int
		var metric string
		var value float64
		var at int64
		if err := rows.Scan(&step, &metric, &value, &at); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"step": step, "metric": metric, "value": value, "at": time.Unix(0, at).UTC()})
	}
	return items, rows.Err()
}

// RegisterModel promotes an artifact to a versioned model.
type Model struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"project_id"`
	Name           string    `json:"name"`
	ArtifactID     string    `json:"artifact_id"`
	Format         string    `json:"format,omitempty"`
	SourceTraining string    `json:"source_training,omitempty"`
	Checksum       string    `json:"checksum,omitempty"`
	SizeBytes      int64     `json:"size_bytes"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
}

func (s *TrainStore) RegisterModel(ctx context.Context, projectID, name, artifactID, format, sourceTraining, checksum string, size int64, actor string) (Model, error) {
	if !nameRe.MatchString(name) {
		return Model{}, fmt.Errorf("%w: model names must be lowercase letters, digits and dashes", ErrBadInput)
	}
	if !strings.HasPrefix(artifactID, "art-") {
		return Model{}, fmt.Errorf("%w: model registration requires a project artifact", ErrBadInput)
	}
	base := strings.ReplaceAll(strings.ToLower(name), "-", "")
	if len(base) > 8 {
		base = base[:8]
	}
	id := fmt.Sprintf("mdl-%s-%06d", base, time.Now().UTC().UnixNano()%1000000)
	now := time.Now().UTC().UnixNano()
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO models(id,project_id,name,artifact_id,format,source_training,checksum,size_bytes,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
		id, projectID, name, artifactID, format, sourceTraining, checksum, size, actor, now); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Model{}, fmt.Errorf("%w: model name already exists", ErrConflict)
		}
		return Model{}, err
	}
	return s.GetModel(ctx, projectID, id)
}

func (s *TrainStore) GetModel(ctx context.Context, projectID, id string) (Model, error) {
	var m Model
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id,project_id,name,artifact_id,format,source_training,checksum,size_bytes,created_by,created_at FROM models WHERE id=? AND project_id=?",
		id, projectID).Scan(&m.ID, &m.ProjectID, &m.Name, &m.ArtifactID, &m.Format, &m.SourceTraining, &m.Checksum, &m.SizeBytes, &m.CreatedBy, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, ErrNotFound
	}
	m.CreatedAt = time.Unix(0, createdAt).UTC()
	return m, err
}

func (s *TrainStore) ListModels(ctx context.Context, projectID string) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id,project_id,name,artifact_id,format,source_training,checksum,size_bytes,created_by,created_at FROM models WHERE project_id=? ORDER BY created_at DESC LIMIT 100", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Model, 0)
	for rows.Next() {
		var m Model
		var createdAt int64
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Name, &m.ArtifactID, &m.Format, &m.SourceTraining, &m.Checksum, &m.SizeBytes, &m.CreatedBy, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(0, createdAt).UTC()
		items = append(items, m)
	}
	return items, rows.Err()
}

// --- Training Checkpoints -----------------------------------------------------

type TrainingCheckpoint struct {
	ID         string    `json:"id"`
	TrainingID string    `json:"training_id"`
	ArtifactID string    `json:"artifact_id"`
	Step       int       `json:"step"`
	Metrics    string    `json:"metrics"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

const checkpointColumns = "id,training_id,artifact_id,step,metrics,created_by,created_at"

func scanCheckpoint(row interface{ Scan(...any) error }) (TrainingCheckpoint, error) {
	var c TrainingCheckpoint
	var createdAt int64
	if err := row.Scan(&c.ID, &c.TrainingID, &c.ArtifactID, &c.Step, &c.Metrics, &c.CreatedBy, &createdAt); err != nil {
		return c, err
	}
	c.CreatedAt = time.Unix(0, createdAt).UTC()
	return c, nil
}

// CreateCheckpoint records a checkpoint artifact for a training job.
// The artifact must already exist in the artifact store.
func (s *TrainStore) CreateCheckpoint(ctx context.Context, trainingID, artifactID string, step int, metrics map[string]float64, actor string) (TrainingCheckpoint, error) {
	metricsJSON, _ := json.Marshal(metrics)
	id := "chk-" + strings.TrimPrefix(uuid.New().String(), "uuid-")
	now := time.Now().UTC().UnixNano()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO training_checkpoints(id,training_id,artifact_id,step,metrics,created_by,created_at) VALUES(?,?,?,?,?,?,?)",
		id, trainingID, artifactID, step, string(metricsJSON), actor, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return TrainingCheckpoint{}, fmt.Errorf("%w: checkpoint for this step already exists", ErrConflict)
		}
		return TrainingCheckpoint{}, err
	}
	return s.GetCheckpoint(ctx, trainingID, id)
}

// GetCheckpoint retrieves a specific checkpoint.
func (s *TrainStore) GetCheckpoint(ctx context.Context, trainingID, id string) (TrainingCheckpoint, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+checkpointColumns+" FROM training_checkpoints WHERE id=? AND training_id=?", id, trainingID)
	c, err := scanCheckpoint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return TrainingCheckpoint{}, ErrNotFound
	}
	return c, err
}

// ListCheckpoints lists all checkpoints for a training job.
func (s *TrainStore) ListCheckpoints(ctx context.Context, trainingID string) ([]TrainingCheckpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+checkpointColumns+" FROM training_checkpoints WHERE training_id=? ORDER BY created_at DESC LIMIT 100", trainingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TrainingCheckpoint, 0)
	for rows.Next() {
		c, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

// ResumeFromCheckpoint creates a new training job that resumes from a checkpoint.
// The new job will have the checkpoint artifact as its model_artifact and resume_from set to the checkpoint ID.
func (s *TrainStore) ResumeFromCheckpoint(ctx context.Context, projectID, name, checkpointID string, actor string) (TrainingJob, error) {
	chk, err := s.GetCheckpoint(ctx, "", checkpointID)
	if err != nil {
		return TrainingJob{}, err
	}
	// Get the original training job to get its config
	origJob, err := s.GetTraining(ctx, projectID, chk.TrainingID)
	if err != nil {
		return TrainingJob{}, err
	}
	// Get the original workload to get image and resources
	origWorkload, err := s.core.Get(ctx, origJob.WorkloadID)
	if err != nil {
		return TrainingJob{}, err
	}
	// Submit a new training job that resumes from the checkpoint
	// The checkpoint artifact becomes the model_artifact for the new job
	// The resume_from field tracks which checkpoint we're resuming from
	return s.SubmitTraining(ctx, projectID, name, origWorkload.Submission.Image, nil, origWorkload.Submission.ImageDigest,
		origWorkload.Submission.Resources, origJob.DatasetArtifact, chk.ArtifactID, chk.ID, "", "", nil, 0, 0, actor)
}

// UpdateCheckpointInfo updates the training job's checkpoint reference.
func (s *TrainStore) UpdateCheckpointInfo(ctx context.Context, trainingID, checkpointArtifact string, checkpointStep int, checkpointPath string) error {
	now := time.Now().UTC().UnixNano()
	_, err := s.db.ExecContext(ctx,
		"UPDATE training_jobs SET checkpoint_artifact=?, checkpoint_step=?, checkpoint_path=?, updated_at=? WHERE id=?",
		checkpointArtifact, checkpointStep, checkpointPath, now, trainingID)
	return err
}

// --- Infer --------------------------------------------------------------------

type InferStore struct {
	db   *sql.DB
	core *core.Store
}

func NewInferStore(db *sql.DB, coreStore *core.Store) *InferStore {
	return &InferStore{db: db, core: coreStore}
}

type InferService struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	ModelID     string    `json:"model_id"`
	Runtime     string    `json:"runtime"`
	UpstreamURL string    `json:"upstream_url,omitempty"`
	WorkloadID  string    `json:"workload_id,omitempty"`
	State       string    `json:"state"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const inferColumns = "id,project_id,name,model_id,runtime,upstream_url,workload_id,state,created_by,created_at,updated_at"

func scanInfer(row interface{ Scan(...any) error }) (InferService, error) {
	var svc InferService
	var createdAt, updatedAt int64
	if err := row.Scan(&svc.ID, &svc.ProjectID, &svc.Name, &svc.ModelID, &svc.Runtime, &svc.UpstreamURL,
		&svc.WorkloadID, &svc.State, &svc.CreatedBy, &createdAt, &updatedAt); err != nil {
		return svc, err
	}
	svc.CreatedAt = time.Unix(0, createdAt).UTC()
	svc.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return svc, nil
}

// CreateInferService schedules the model server. Runtime detection is the
// caller's job: the coordinator records the resolved runtime and upstream.
func (s *InferStore) CreateInferService(ctx context.Context, projectID, name, modelID, runtimeKind, upstreamURL, image string, imageDigest string, resources core.Resources, actor string) (InferService, error) {
	if !nameRe.MatchString(name) {
		return InferService{}, fmt.Errorf("%w: service names must be lowercase letters, digits and dashes", ErrBadInput)
	}
	switch runtimeKind {
	case "ollama", "llamacpp", "vllm", "openai-compatible", "proxy":
	default:
		return InferService{}, fmt.Errorf("%w: runtime must be ollama, llamacpp, vllm, openai-compatible or proxy", ErrBadInput)
	}
	submission := core.Submission{
		Service:     "infer",
		Kind:        "service",
		Image:       image,
		ImageDigest: imageDigest,
		Network:     "private",
		Resources:   resources,
		// The health path is runtime specific; the caller supplies readiness
		// through the envelope when the runtime supports it.
		HealthCheck: &core.HealthCheck{Kind: "http", Port: 8080, Path: "/health"},
	}
	submission.SetScope(projectID, actor)
	if err := submission.Normalize(); err != nil {
		return InferService{}, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	workload, _, err := s.core.SubmitForProjectAs(ctx, projectID, actor, submission, "infer:"+name)
	if err != nil {
		if errors.Is(err, core.ErrConflict) {
			return InferService{}, fmt.Errorf("%w: an inference service with this name already exists", ErrConflict)
		}
		return InferService{}, err
	}
	now := time.Now().UTC().UnixNano()
	id := "inf-" + strings.TrimPrefix(workload.ID, "wl-")
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO infer_services(id,project_id,name,model_id,runtime,upstream_url,workload_id,state,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'running',?,?,?)",
		id, projectID, name, modelID, runtimeKind, upstreamURL, workload.ID, actor, now, now); err != nil {
		return InferService{}, err
	}
	return s.GetInferService(ctx, projectID, id)
}

func (s *InferStore) GetInferService(ctx context.Context, projectID, id string) (InferService, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+inferColumns+" FROM infer_services WHERE id=? AND project_id=?", id, projectID)
	svc, err := scanInfer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return InferService{}, ErrNotFound
	}
	return svc, err
}

func (s *InferStore) ListInferServices(ctx context.Context, projectID string) ([]InferService, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+inferColumns+" FROM infer_services WHERE project_id=? ORDER BY created_at DESC LIMIT 100", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]InferService, 0)
	for rows.Next() {
		svc, err := scanInfer(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, svc)
	}
	return items, rows.Err()
}

func (s *InferStore) StopInferService(ctx context.Context, projectID, id string) error {
	svc, err := s.GetInferService(ctx, projectID, id)
	if err != nil {
		return err
	}
	for {
		workload, err := s.core.Get(ctx, svc.WorkloadID)
		if err != nil {
			return err
		}
		if workload.State.Terminal() || workload.CancelRequested {
			return nil
		}
		workload.CancelRequested = true
		if err := s.core.Save(ctx, workload, false); !errors.Is(err, core.ErrConflict) {
			return err
		}
	}
}
