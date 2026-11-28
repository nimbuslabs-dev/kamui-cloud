package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/backup"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/drop"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/ml"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/objectstore"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/ops"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/routing"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/runtime"
)

// servicesHandler implements the Store/Backup, Train/Infer, SRE/collaboration
// and Drop APIs over the shared identity and execution stack.
type servicesHandler struct {
	objects *objectstore.Store
	backups *backup.Store
	train   *ml.TrainStore
	infer   *ml.InferStore
	ops     *ops.Store
	drop    *drop.Store
	ids     *identity.Store
	engine  *core.Engine
	hub     *routing.Hub
	routes  *runtime.Store
}

func newServicesHandler(objects *objectstore.Store, backups *backup.Store, train *ml.TrainStore,
	infer *ml.InferStore, opsStore *ops.Store, dropStore *drop.Store, ids *identity.Store, engine *core.Engine, hub *routing.Hub, routes *runtime.Store) *servicesHandler {
	return &servicesHandler{
		objects: objects, backups: backups, train: train, infer: infer,
		ops: opsStore, drop: dropStore, ids: ids, engine: engine, hub: hub, routes: routes,
	}
}

func (h *servicesHandler) register(mux *http.ServeMux) {
	// Object storage
	mux.HandleFunc("POST /v1/projects/{id}/buckets", h.CreateBucket)
	mux.HandleFunc("GET /v1/projects/{id}/buckets", h.ListBuckets)
	mux.HandleFunc("DELETE /v1/projects/{id}/buckets/{bucket}", h.DeleteBucket)
	mux.HandleFunc("PUT /v1/projects/{id}/buckets/{bucket}/objects/{key...}", h.PutObject)
	mux.HandleFunc("GET /v1/projects/{id}/buckets/{bucket}/objects/{key...}", h.GetObject)
	mux.HandleFunc("DELETE /v1/projects/{id}/buckets/{bucket}/objects/{key...}", h.DeleteObject)
	mux.HandleFunc("GET /v1/projects/{id}/buckets/{bucket}/objects", h.ListObjects)
	// Backup and restore
	mux.HandleFunc("POST /v1/backups/metadata", h.CreateMetadataBackup)
	mux.HandleFunc("POST /v1/projects/{id}/backups/objects", h.CreateObjectBackup)
	mux.HandleFunc("POST /v1/projects/{id}/backups/volume", h.CreateVolumeBackup)
	mux.HandleFunc("GET /v1/backups", h.ListBackups)
	mux.HandleFunc("GET /v1/backups/{bck}", h.GetBackup)
	mux.HandleFunc("GET /v1/backups/{bck}/content", h.DownloadBackup)
	// Train
	mux.HandleFunc("POST /v1/projects/{id}/training", h.SubmitTraining)
	mux.HandleFunc("GET /v1/projects/{id}/training", h.ListTrainings)
	mux.HandleFunc("GET /v1/projects/{id}/training/{trn}", h.GetTraining)
	mux.HandleFunc("GET /v1/projects/{id}/training/{trn}/metrics", h.ListTrainingMetrics)
	mux.HandleFunc("POST /v1/projects/{id}/training/{trn}/cancel", h.CancelTraining)
	mux.HandleFunc("POST /v1/projects/{id}/training/{trn}/collect", h.CollectTrainingOutputs)
	// Training checkpoints
	mux.HandleFunc("GET /v1/projects/{id}/training/{trn}/checkpoints", h.ListCheckpoints)
	mux.HandleFunc("POST /v1/projects/{id}/training/{trn}/checkpoints", h.CreateCheckpoint)
	mux.HandleFunc("POST /v1/projects/{id}/training/{trn}/resume", h.ResumeFromCheckpoint)
	mux.HandleFunc("POST /v1/projects/{id}/models", h.RegisterModel)
	mux.HandleFunc("GET /v1/projects/{id}/models", h.ListModels)
	mux.HandleFunc("GET /v1/projects/{id}/models/{model}", h.GetModel)
	// Infer
	mux.HandleFunc("POST /v1/projects/{id}/inference", h.CreateInferService)
	mux.HandleFunc("GET /v1/projects/{id}/inference", h.ListInferServices)
	mux.HandleFunc("GET /v1/projects/{id}/inference/{svc}", h.GetInferService)
	mux.HandleFunc("POST /v1/projects/{id}/inference/{svc}/stop", h.StopInferService)
	for _, method := range []string{"GET", "POST"} {
		mux.HandleFunc(method+" /v1/projects/{id}/inference/{svc}/proxy/{path...}", h.ProxyInference)
		mux.HandleFunc(method+" /v1/projects/{id}/inference/{svc}/proxy", h.ProxyInference)
	}
	// SRE / collaboration
	mux.HandleFunc("PUT /v1/projects/{id}/quotas", h.SetQuota)
	mux.HandleFunc("GET /v1/projects/{id}/quotas", h.GetQuota)
	mux.HandleFunc("GET /v1/projects/{id}/usage", h.ListUsage)
	mux.HandleFunc("POST /v1/projects/{id}/alerts", h.CreateAlertRule)
	mux.HandleFunc("GET /v1/projects/{id}/alerts", h.ListAlertRules)
	mux.HandleFunc("DELETE /v1/alerts/{alr}", h.DeleteAlertRule)
	mux.HandleFunc("GET /v1/alerts/{alr}/events", h.ListAlertEvents)
	mux.HandleFunc("GET /v1/projects/{id}/databases", h.ListDatabases)
	mux.HandleFunc("POST /v1/projects/{id}/databases", h.CreateDatabase)
	mux.HandleFunc("GET /v1/projects/{id}/databases/{db}", h.GetDatabase)
	mux.HandleFunc("POST /v1/projects/{id}/databases/{db}/stop", h.StopDatabase)
	mux.HandleFunc("POST /v1/projects/{id}/databases/{db}/backup", h.BackupDatabase)
	mux.HandleFunc("POST /v1/projects/{id}/databases/{db}/restore", h.RestoreDatabase)
	mux.HandleFunc("DELETE /v1/projects/{id}/databases/{db}", h.DeleteDatabase)
	// Backup restore
	mux.HandleFunc("POST /v1/backups/{bck}/restore", h.RestoreBackup)
	// Drop
	mux.HandleFunc("POST /v1/drop/transfers", h.CreateTransfer)
	mux.HandleFunc("GET /v1/drop/transfers", h.ListTransfers)
	mux.HandleFunc("GET /v1/drop/transfers/{xfr}", h.GetTransfer)
	mux.HandleFunc("POST /v1/drop/transfers/{xfr}/approve", h.ApproveTransfer)
	mux.HandleFunc("POST /v1/drop/transfers/{xfr}/reject", h.RejectTransfer)
	mux.HandleFunc("POST /v1/drop/transfers/{xfr}/cancel", h.CancelTransfer)
	mux.HandleFunc("GET /v1/drop/transfers/{xfr}/content", h.DownloadTransfer)
}

func (h *servicesHandler) authorize(w http.ResponseWriter, r *http.Request, projectID string, perm identity.Permission) (identity.Principal, bool) {
	if h.ids == nil {
		return auth.OperatorPrincipal(), true
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return identity.Principal{}, false
	}
	if _, err := h.ids.Authorize(r.Context(), p, projectID, perm); err != nil {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission for this project resource")
		return identity.Principal{}, false
	}
	return p, true
}

func servicesError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, objectstore.ErrNotFound), errors.Is(err, backup.ErrNotFound),
		errors.Is(err, ml.ErrNotFound), errors.Is(err, ops.ErrNotFound), errors.Is(err, drop.ErrNotFound):
		apierr.WriteError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, objectstore.ErrConflict), errors.Is(err, ml.ErrConflict), errors.Is(err, drop.ErrConflict):
		apierr.WriteError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, objectstore.ErrTooLarge), errors.Is(err, drop.ErrTooLarge), errors.Is(err, backup.ErrTooBig):
		apierr.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, backup.ErrNotImplemented), errors.Is(err, ml.ErrResumeUnsupported):
		apierr.WriteError(w, http.StatusNotImplemented, "not_implemented", err.Error())
	case errors.Is(err, objectstore.ErrQuota):
		apierr.WriteError(w, http.StatusInsufficientStorage, "quota_exceeded", err.Error())
	case errors.Is(err, backup.ErrForbidden), errors.Is(err, drop.ErrForbidden):
		apierr.WriteError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, objectstore.ErrBadInput), errors.Is(err, backup.ErrBadInput),
		errors.Is(err, ml.ErrBadInput), errors.Is(err, ops.ErrBadInput), errors.Is(err, drop.ErrBadInput):
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		slog.Warn("service operation failed", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "the requested service operation is temporarily unavailable")
	}
}

// --- Object storage -----------------------------------------------------------

func (h *servicesHandler) CreateBucket(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if err := readJSON(w, r, 4<<10, &input); err != nil || input.Name == "" {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "a bucket name is required")
		return
	}
	bucket, err := h.objects.CreateBucket(r.Context(), projectID, input.Name, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, bucket)
}

func (h *servicesHandler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	items, err := h.objects.ListBuckets(r.Context(), projectID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) bucket(w http.ResponseWriter, r *http.Request, perm identity.Permission) bool {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, perm); !ok {
		return false
	}
	if _, err := h.objects.GetBucket(r.Context(), projectID, r.PathValue("bucket")); err != nil {
		servicesError(w, err)
		return false
	}
	return true
}

func (h *servicesHandler) PutObject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	bucketID := r.PathValue("bucket")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	bucket, err := h.objects.GetBucketByName(r.Context(), projectID, bucketID)
	if err != nil {
		servicesError(w, err)
		return
	}
	key := strings_ToKey(r.PathValue("key"))
	if !objectstore.ValidObjectKey(key) {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "object keys must be safe forward-slash paths")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, objectstore.MaxObjectBytes+1)
	object, err := h.objects.PutObject(r.Context(), bucket, key, r.Header.Get("Content-Type"), r.Body)
	if err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "store.object.put", "object", bucket.Name+"/"+key, "success", "")
	}
	apierr.WriteJSON(w, http.StatusCreated, object)
}

func (h *servicesHandler) GetObject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	bucketID := r.PathValue("bucket")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	bucket, err := h.objects.GetBucketByName(r.Context(), projectID, bucketID)
	if err != nil {
		servicesError(w, err)
		return
	}
	key := strings_ToKey(r.PathValue("key"))
	if r.URL.Query().Get("metadata") == "true" {
		object, err := h.objects.GetObject(r.Context(), bucket, key)
		if err != nil {
			servicesError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, object)
		return
	}
	reader, object, err := h.objects.OpenObject(r.Context(), bucket, key)
	if err != nil {
		servicesError(w, err)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", object.ContentType)
	w.Header().Set("X-Checksum-Sha256", object.Checksum)
	w.Header().Set("Content-Length", strconv.FormatInt(object.SizeBytes, 10))
	if _, err := io.Copy(w, reader); err != nil {
		slog.Warn("object download failed", "key", key, "error", err)
	}
}

func (h *servicesHandler) DeleteObject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	bucketID := r.PathValue("bucket")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	bucket, err := h.objects.GetBucketByName(r.Context(), projectID, bucketID)
	if err != nil {
		servicesError(w, err)
		return
	}
	key := strings_ToKey(r.PathValue("key"))
	if err := h.objects.DeleteObject(r.Context(), bucket, key); err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "store.object.delete", "object", bucket.Name+"/"+key, "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *servicesHandler) ListObjects(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	bucketID := r.PathValue("bucket")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	bucket, err := h.objects.GetBucketByName(r.Context(), projectID, bucketID)
	if err != nil {
		servicesError(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.objects.ListObjects(r.Context(), bucket, r.URL.Query().Get("after"), limit)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) DeleteBucket(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	if err := h.objects.DeleteBucket(r.Context(), projectID, r.PathValue("bucket")); err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "store.bucket.delete", "bucket", r.PathValue("bucket"), "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Backup / restore ------------------------------------------------------------

func (h *servicesHandler) CreateMetadataBackup(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authorize(w, r, "", identity.PermWorkloadWrite)
	if !ok {
		return
	}
	// Metadata backups are coordinator-wide and administrative.
	if h.ids != nil && !p.User.IsSystemAdmin {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "administrator permission is required for metadata backups")
		return
	}
	backup, err := h.backups.CreateMetadataBackup(r.Context(), p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, backup)
}

func (h *servicesHandler) CreateObjectBackup(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	buckets, err := h.objects.ListBuckets(r.Context(), projectID)
	if err != nil {
		servicesError(w, err)
		return
	}
	var refs []backup.ObjectRef
	bucketByName := make(map[string]objectstore.Bucket, len(buckets))
	for _, bucket := range buckets {
		bucketByName[bucket.Name] = bucket
		for after := ""; ; {
			objects, err := h.objects.ListObjects(r.Context(), bucket, after, 1000)
			if err != nil {
				servicesError(w, err)
				return
			}
			if len(refs)+len(objects) > backup.MaxRestoreEntries {
				servicesError(w, backup.ErrTooBig)
				return
			}
			for _, object := range objects {
				refs = append(refs, backup.ObjectRef{Bucket: bucket.Name, Key: object.Key, Size: object.SizeBytes})
			}
			if len(objects) < 1000 {
				break
			}
			after = objects[len(objects)-1].Key
		}
	}
	result, err := h.backups.CreateObjectBackup(r.Context(), projectID, p.User.ID, refs, func(ctx context.Context, ref backup.ObjectRef) (io.ReadCloser, error) {
		reader, _, err := h.objects.OpenObject(ctx, bucketByName[ref.Bucket], ref.Key)
		return reader, err
	})
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, result)
}

func (h *servicesHandler) CreateVolumeBackup(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		Volume string `json:"volume"`
	}
	if err := readJSON(w, r, 4<<10, &input); err != nil || input.Volume == "" {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "a volume name is required")
		return
	}
	result, err := h.backups.CreateVolumeBackup(r.Context(), projectID, input.Volume, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, result)
}

func (h *servicesHandler) backupScope(w http.ResponseWriter, r *http.Request, perm identity.Permission) (identity.Principal, string, bool) {
	projectID := r.URL.Query().Get("project_id")
	if projectID != "" {
		p, ok := h.authorize(w, r, projectID, perm)
		return p, projectID, ok
	}
	p, ok := h.requirePrincipal(w, r)
	if ok && !p.User.IsSystemAdmin {
		servicesError(w, backup.ErrForbidden)
		ok = false
	}
	return p, "", ok
}

func (h *servicesHandler) ListBackups(w http.ResponseWriter, r *http.Request) {
	_, projectID, ok := h.backupScope(w, r, identity.PermWorkloadRead)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.backups.ListBackups(r.Context(), projectID, limit)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) GetBackup(w http.ResponseWriter, r *http.Request) {
	_, projectID, ok := h.backupScope(w, r, identity.PermWorkloadRead)
	if !ok {
		return
	}
	record, err := h.backups.GetBackup(r.Context(), projectID, r.PathValue("bck"))
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, record)
}

func (h *servicesHandler) DownloadBackup(w http.ResponseWriter, r *http.Request) {
	_, projectID, ok := h.backupScope(w, r, identity.PermWorkloadRead)
	if !ok {
		return
	}
	id := r.PathValue("bck")
	record, err := h.backups.GetBackup(r.Context(), projectID, id)
	if err != nil {
		servicesError(w, err)
		return
	}
	reader, size, err := h.backups.OpenBackup(r.Context(), projectID, id)
	if err != nil {
		servicesError(w, err)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Checksum-Sha256", record.Checksum)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, reader); err != nil {
		slog.Debug("backup download interrupted", "backup_id", id, "error", err)
	}
}

// --- Train -------------------------------------------------------------------------

func (h *servicesHandler) SubmitTraining(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		Name            string            `json:"name"`
		Image           string            `json:"image"`
		ImageDigest     string            `json:"image_digest,omitempty"`
		Args            []string          `json:"args,omitempty"`
		Env             map[string]string `json:"env,omitempty"`
		Resources       core.Resources    `json:"resources"`
		DatasetArtifact string            `json:"dataset_artifact,omitempty"`
		Placement       string            `json:"placement,omitempty"`
		WorkerID        string            `json:"worker_id,omitempty"`
		TimeoutSeconds  int               `json:"timeout_seconds,omitempty"`
		MaxRetries      *int              `json:"max_retries,omitempty"`
	}
	if err := readJSON(w, r, 64<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid training JSON")
		return
	}
	retries := 0
	if input.MaxRetries != nil {
		retries = *input.MaxRetries
	}
	job, err := h.train.SubmitTraining(r.Context(), projectID, input.Name, input.Image, input.Args, input.ImageDigest,
		input.Resources, input.DatasetArtifact, "", "", input.Placement, input.WorkerID, input.Env, input.TimeoutSeconds, retries, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "train.submit", "training", job.ID, "success", job.Name)
	}
	apierr.WriteJSON(w, http.StatusAccepted, job)
}

func (h *servicesHandler) ListTrainings(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	items, err := h.train.ListTrainings(r.Context(), projectID)
	if err != nil {
		servicesError(w, err)
		return
	}
	for i := range items {
		h.train.SyncTrainingState(r.Context(), &items[i])
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) GetTraining(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	job, err := h.train.GetTraining(r.Context(), projectID, r.PathValue("trn"))
	if err != nil {
		servicesError(w, err)
		return
	}
	h.train.SyncTrainingState(r.Context(), &job)
	var logs any
	if h.engine != nil && job.WorkloadID != "" {
		if tail, err := h.engine.Logs(r.Context(), job.WorkloadID); err == nil {
			logs = tail
		}
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"job": job, "logs": logs})
}

func (h *servicesHandler) ListTrainingMetrics(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	job, err := h.train.GetTraining(r.Context(), projectID, r.PathValue("trn"))
	if err != nil {
		servicesError(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.train.ListMetrics(r.Context(), job.ID, limit)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) CancelTraining(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	job, err := h.train.GetTraining(r.Context(), projectID, r.PathValue("trn"))
	if err != nil {
		servicesError(w, err)
		return
	}
	if h.engine == nil {
		servicesError(w, errors.New("scheduler unavailable"))
		return
	}
	workload, err := h.engine.Cancel(r.Context(), job.WorkloadID)
	if err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "train.cancel", "training", job.ID, "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, workload)
}

// CollectTrainingOutputs gathers declared outputs from a finished local
// attempt, ingests observed metrics and registers the model artifact blob.
// Remote-worker outputs are staged by the agent; their coordinator-side
// registration is a documented PARTIAL for this checkpoint.
func (h *servicesHandler) CollectTrainingOutputs(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	job, err := h.train.GetTraining(r.Context(), projectID, r.PathValue("trn"))
	if err != nil {
		servicesError(w, err)
		return
	}
	h.train.SyncTrainingState(r.Context(), &job)
	if h.engine == nil {
		servicesError(w, errors.New("scheduler unavailable"))
		return
	}
	workload, err := h.engine.Repo.Get(r.Context(), job.WorkloadID)
	if err != nil {
		servicesError(w, err)
		return
	}
	if !workload.State.Terminal() {
		apierr.WriteError(w, http.StatusConflict, "still_running", "outputs can be collected only from a finished attempt")
		return
	}
	collector, ok := h.engine.Executor.(OutputCollector)
	if !ok {
		apierr.WriteError(w, http.StatusServiceUnavailable, "collect_unavailable", "output collection requires a shared local Docker daemon")
		return
	}
	var ingested int
	var metricLines []string
	for _, output := range workload.Submission.Outputs {
		data, err := collector.CollectOutput(r.Context(), job.WorkloadID, workload.Attempt, output)
		if err != nil {
			continue // missing outputs are not fatal; metrics may be absent
		}
		if strings.HasSuffix(output, "metrics.jsonl") {
			metricLines = strings.Split(strings.TrimSpace(string(data)), "\n")
			tmp, err := os.CreateTemp("", "metrics-*.jsonl")
			if err == nil {
				_, _ = tmp.WriteString(string(data))
				tmp.Close()
				ingested, err = h.train.IngestMetrics(r.Context(), job.ID, tmp.Name())
				if err != nil {
					os.Remove(tmp.Name())
					servicesError(w, err)
					return
				}
				os.Remove(tmp.Name())
			}
		}
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{
		"ingested_metrics": ingested,
		"metric_lines":     len(metricLines),
		"collector":        p.User.ID,
	})
}

// ListCheckpoints lists all checkpoints for a training job.
func (h *servicesHandler) ListCheckpoints(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	items, err := h.train.ListCheckpoints(r.Context(), projectID, r.PathValue("trn"))
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

// CreateCheckpoint creates a checkpoint for a training job.
func (h *servicesHandler) CreateCheckpoint(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		ArtifactID string             `json:"artifact_id"`
		Step       int                `json:"step"`
		Metrics    map[string]float64 `json:"metrics,omitempty"`
	}
	if err := readJSON(w, r, 64<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid checkpoint JSON")
		return
	}
	if input.ArtifactID == "" || input.Step <= 0 {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "artifact_id and step are required")
		return
	}
	chk, err := h.train.CreateCheckpoint(r.Context(), projectID, r.PathValue("trn"), input.ArtifactID, input.Step, input.Metrics, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, chk)
}

// ResumeFromCheckpoint creates a new training job that resumes from a checkpoint.
func (h *servicesHandler) ResumeFromCheckpoint(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		CheckpointID string `json:"checkpoint_id"`
		Name         string `json:"name"`
	}
	if err := readJSON(w, r, 8<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid resume request")
		return
	}
	if input.CheckpointID == "" || input.Name == "" {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "checkpoint_id and name are required")
		return
	}
	job, err := h.train.ResumeFromCheckpoint(r.Context(), projectID, r.PathValue("trn"), input.Name, input.CheckpointID, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusAccepted, job)
}

// OutputCollector is implemented by the local Docker executor.
type OutputCollector interface {
	CollectOutput(ctx context.Context, workloadID string, attempt int, containerPath string) ([]byte, error)
}

func (h *servicesHandler) RegisterModel(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		Name           string `json:"name"`
		ArtifactID     string `json:"artifact_id"`
		Format         string `json:"format,omitempty"`
		SourceTraining string `json:"source_training,omitempty"`
		Checksum       string `json:"checksum,omitempty"`
		SizeBytes      int64  `json:"size_bytes"`
	}
	if err := readJSON(w, r, 16<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid model JSON")
		return
	}
	model, err := h.train.RegisterModel(r.Context(), projectID, input.Name, input.ArtifactID, input.Format, input.SourceTraining, input.Checksum, input.SizeBytes, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, model)
}

func (h *servicesHandler) ListModels(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	items, err := h.train.ListModels(r.Context(), projectID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) GetModel(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	model, err := h.train.GetModel(r.Context(), projectID, r.PathValue("model"))
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, model)
}

// --- Infer ---------------------------------------------------------------------------

func (h *servicesHandler) CreateInferService(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		Name        string         `json:"name"`
		ModelID     string         `json:"model_id,omitempty"`
		Runtime     string         `json:"runtime"`
		UpstreamURL string         `json:"upstream_url,omitempty"`
		Image       string         `json:"image"`
		ImageDigest string         `json:"image_digest,omitempty"`
		Resources   core.Resources `json:"resources"`
	}
	if err := readJSON(w, r, 32<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid inference JSON")
		return
	}
	svc, err := h.infer.CreateInferService(r.Context(), projectID, input.Name, input.ModelID, input.Runtime, input.UpstreamURL, input.Image, input.ImageDigest, input.Resources, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "infer.create", "inference", svc.ID, "success", svc.Name)
	}
	apierr.WriteJSON(w, http.StatusCreated, svc)
}

func (h *servicesHandler) ListInferServices(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	items, err := h.infer.ListInferServices(r.Context(), projectID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) GetInferService(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	svc, err := h.infer.GetInferService(r.Context(), projectID, r.PathValue("svc"))
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, svc)
}

func (h *servicesHandler) StopInferService(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	if err := h.infer.StopInferService(r.Context(), projectID, r.PathValue("svc")); err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "infer.stop", "inference", r.PathValue("svc"), "success", "")
	}
	apierr.WriteJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// ProxyInference forwards authorized project requests to the inference
// service's route. Upstream destinations come from the route registry, never
// from client-supplied URLs, which prevents arbitrary internal proxying.
func (h *servicesHandler) ProxyInference(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	svc, err := h.infer.GetInferService(r.Context(), projectID, r.PathValue("svc"))
	if err != nil {
		servicesError(w, err)
		return
	}
	if h.engine == nil || h.routes == nil {
		apierr.WriteError(w, http.StatusServiceUnavailable, "proxy_unavailable", "service routing is unavailable")
		return
	}
	route, err := h.routes.RouteLookup(r.Context(), svc.WorkloadID)
	if err != nil || route.ProjectID != projectID {
		apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "the inference service has no live route")
		return
	}
	item, err := h.engine.Repo.Get(r.Context(), svc.WorkloadID)
	if err != nil || !liveServiceRoute(route, item, time.Now()) {
		apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "the service assignment is no longer live")
		return
	}
	transport := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 60 * time.Second}
	host := "kamui-service"
	if route.WorkerID == core.LocalWorker {
		resolver, ok := h.engine.Executor.(ContainerResolver)
		if !ok {
			apierr.WriteError(w, http.StatusServiceUnavailable, "proxy_unavailable", "local service resolver is unavailable")
			return
		}
		addr, err := resolver.ContainerServiceAddress(r.Context(), route.WorkloadID, route.Attempt)
		if err != nil {
			apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "local service is unreachable")
			return
		}
		host = net.JoinHostPort(addr, strconv.Itoa(route.Port))
	} else {
		if h.hub == nil || h.hub.Get(route.WorkerID) == nil {
			apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "the assigned worker has no live tunnel")
			return
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			tunnel := h.hub.Get(route.WorkerID)
			if tunnel == nil {
				return nil, routing.ErrTunnelClosed
			}
			return tunnel.DialContext(ctx, route.WorkloadID, route.Fence, route.Port)
		}
	}
	proxyService(w, r, host, transport)

}

// --- SRE / collaboration --------------------------------------------------------------

func (h *servicesHandler) SetQuota(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermProjectUpdate)
	if !ok {
		return
	}
	var input ops.Quota
	input.ProjectID = projectID
	if err := readJSON(w, r, 8<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid quota JSON")
		return
	}
	quota, err := h.ops.SetQuota(r.Context(), projectID, input, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, quota)
}

func (h *servicesHandler) GetQuota(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	quota, err := h.ops.GetQuota(r.Context(), projectID)
	if err != nil {
		servicesError(w, err)
		return
	}
	usage, err := h.ops.UsageSnapshot(r.Context(), projectID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"quota": quota, "usage": usage})
}

func (h *servicesHandler) ListUsage(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.ops.ListUsage(r.Context(), projectID, limit)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) CreateAlertRule(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermProjectUpdate)
	if !ok {
		return
	}
	var input struct {
		Kind       string `json:"kind"`
		Target     string `json:"target,omitempty"`
		Threshold  int    `json:"threshold"`
		WebhookURL string `json:"webhook_url,omitempty"`
	}
	if err := readJSON(w, r, 8<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid alert JSON")
		return
	}
	rule, err := h.ops.CreateAlertRule(r.Context(), projectID, input.Kind, input.Target, input.Threshold, input.WebhookURL, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, rule)
}

func (h *servicesHandler) ListAlertRules(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	items, err := h.ops.ListAlertRules(r.Context(), projectID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) DeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("alr")
	rule, err := h.ops.GetAlertRule(r.Context(), id)
	if err != nil {
		servicesError(w, err)
		return
	}
	if _, ok := h.authorize(w, r, rule.ProjectID, identity.PermProjectUpdate); !ok {
		return
	}
	if err := h.ops.DeleteAlertRule(r.Context(), id); err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *servicesHandler) ListAlertEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("alr")
	rule, err := h.ops.GetAlertRule(r.Context(), id)
	if err != nil {
		servicesError(w, err)
		return
	}
	if _, ok := h.authorize(w, r, rule.ProjectID, identity.PermWorkloadRead); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.ops.ListAlertEvents(r.Context(), id, limit)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

// --- Databases -----------------------------------------------------------------------

func (h *servicesHandler) CreateDatabase(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		Name      string         `json:"name"`
		Engine    string         `json:"engine"`
		Version   string         `json:"version,omitempty"`
		Resources core.Resources `json:"resources"`
	}
	if err := readJSON(w, r, 32<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid database JSON")
		return
	}
	database, err := h.ops.CreateDatabase(r.Context(), projectID, input.Name, input.Engine, input.Version, input.Resources, p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "database.create", "database", database.ID, "success", database.Name)
	}
	apierr.WriteJSON(w, http.StatusCreated, database)
}

func (h *servicesHandler) ListDatabases(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	items, err := h.ops.ListDatabases(r.Context(), projectID)
	if err != nil {
		servicesError(w, err)
		return
	}
	for i := range items {
		h.ops.SyncDatabaseState(r.Context(), &items[i])
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) GetDatabase(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	database, err := h.ops.GetDatabase(r.Context(), projectID, r.PathValue("db"))
	if err != nil {
		servicesError(w, err)
		return
	}
	h.ops.SyncDatabaseState(r.Context(), &database)
	var logs any
	if h.engine != nil && database.WorkloadID != "" {
		if tail, err := h.engine.Logs(r.Context(), database.WorkloadID); err == nil {
			logs = tail
		}
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"database": database, "logs": logs})
}

func (h *servicesHandler) StopDatabase(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	if err := h.ops.StopDatabase(r.Context(), projectID, r.PathValue("db")); err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "database.stop", "database", r.PathValue("db"), "success", "")
	}
	apierr.WriteJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (h *servicesHandler) DeleteDatabase(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	if err := h.ops.DeleteDatabase(r.Context(), projectID, r.PathValue("db")); err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "database.delete", "database", r.PathValue("db"), "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// BackupDatabase creates a PostgreSQL dump and returns it as binary data.
func (h *servicesHandler) BackupDatabase(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead)
	if !ok {
		return
	}
	dbID := r.PathValue("db")
	var buf bytes.Buffer
	if err := h.ops.DatabaseBackup(r.Context(), projectID, dbID, &buf); err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "database.backup", "database", dbID, "success", "")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.dump\"", dbID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// RestoreDatabase restores a PostgreSQL dump from the request body.
// By default, restores to an isolated database. Use ?force=true to overwrite the existing database (requires explicit authorization).
func (h *servicesHandler) RestoreDatabase(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	dbID := r.PathValue("db")
	force := false
	if r.URL.Query().Get("force") == "true" {
		// Force restore requires explicit admin authorization
		if p.User.IsSystemAdmin {
			force = true
		} else {
			servicesError(w, fmt.Errorf("%w: force restore requires system administrator authorization", identity.ErrForbidden))
			return
		}
	}
	if err := h.ops.DatabaseRestore(r.Context(), projectID, dbID, r.Body, force); err != nil {
		servicesError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "database.restore", "database", dbID, "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "force": force})
}

// --- Backup restore handlers ----------------------------------------------------

// RestoreBackup handles restore for all backup types based on the backup kind.
func (h *servicesHandler) RestoreBackup(w http.ResponseWriter, r *http.Request) {
	p, projectID, ok := h.backupScope(w, r, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	id := r.PathValue("bck")
	record, err := h.backups.GetBackup(r.Context(), projectID, id)
	if err != nil {
		servicesError(w, err)
		return
	}
	// A restore accepts an empty request. The coordinator selects the destination.
	if r.ContentLength != 0 {
		var input struct{}
		if err := readJSON(w, r, 4<<10, &input); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "restore does not accept a destination")
			return
		}
	}
	var location string
	switch record.Kind {
	case "metadata":
		location, err = h.backups.RestoreMetadata(r.Context(), p, id)
	case "objects":
		location, err = h.backups.RestoreObjectBackup(r.Context(), projectID, id)
	case "volume":
		err = h.backups.RestoreVolumeBackup(r.Context(), projectID, id)
	default:
		err = backup.ErrBadInput
	}
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]string{"restored_to": location})
}

// --- Drop -----------------------------------------------------------------------

func (h *servicesHandler) CreateTransfer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	recipient := r.URL.Query().Get("recipient")
	filename := r.URL.Query().Get("filename")
	ttlSeconds, _ := strconv.Atoi(r.URL.Query().Get("ttl_seconds"))
	r.Body = http.MaxBytesReader(w, r.Body, drop.MaxTransferBytes+1)
	transfer, err := h.drop.CreateTransfer(r.Context(), p.User.ID, recipient, filename, timeFromSeconds(ttlSeconds), r.Body)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, transfer)
}

func timeFromSeconds(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

// strings_ToKey reassembles a wildcard path parameter into an object key.
func strings_ToKey(raw string) string { return raw }

func liveServiceRoute(route runtime.ServiceRoute, item core.Workload, now time.Time) bool {
	a := item.Assignment
	return item.State == core.Running && item.ProjectID == route.ProjectID && item.ID == route.WorkloadID &&
		a != nil && a.WorkerID == route.WorkerID && item.Attempt == route.Attempt && a.Fence == route.Fence &&
		a.LeaseUntil.After(now) && route.ExpiresAt.After(now)
}

func proxyService(w http.ResponseWriter, r *http.Request, host string, transport http.RoundTripper) {
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(&url.URL{Scheme: "http", Host: host})
			req.Out.URL.Path = "/" + req.In.PathValue("path")
			req.Out.URL.RawPath = ""
			req.Out.Header.Del("Authorization")
			req.Out.Header.Del("Cookie")
			req.Out.Header.Del("X-Kamui-Token")
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			res.Header.Set("Cache-Control", "no-store")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			apierr.WriteError(w, http.StatusBadGateway, "service_unavailable", "the service upstream could not complete the request")
		},
	}
	if r.Header.Get("Upgrade") != "" {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "this service route does not support upgrades")
		return
	}
	proxy.ServeHTTP(w, r)
}
func (h *servicesHandler) requirePrincipal(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	if h.ids == nil {
		return auth.OperatorPrincipal(), true
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return identity.Principal{}, false
	}
	return p, true
}

func (h *servicesHandler) ListTransfers(w http.ResponseWriter, r *http.Request) {
	p, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	items, err := h.drop.ListTransfers(r.Context(), p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *servicesHandler) GetTransfer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	transfer, err := h.drop.GetTransferAs(r.Context(), r.PathValue("xfr"), p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, transfer)
}

func (h *servicesHandler) ApproveTransfer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	transfer, err := h.drop.Approve(r.Context(), r.PathValue("xfr"), p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, transfer)
}

func (h *servicesHandler) RejectTransfer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	transfer, err := h.drop.Reject(r.Context(), r.PathValue("xfr"), p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, transfer)
}

func (h *servicesHandler) CancelTransfer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.drop.Cancel(r.Context(), r.PathValue("xfr"), p.User.ID); err != nil {
		servicesError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *servicesHandler) DownloadTransfer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	reader, transfer, err := h.drop.OpenForDownload(r.Context(), r.PathValue("xfr"), p.User.ID)
	if err != nil {
		servicesError(w, err)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Checksum-Sha256", transfer.Checksum)
	w.Header().Set("Content-Length", strconv.FormatInt(transfer.SizeBytes, 10))
	w.Header().Set("Content-Disposition", "attachment; filename=\""+transfer.Filename+"\"")
	if _, err := io.Copy(w, reader); err != nil {
		slog.Warn("transfer download failed", "transfer", transfer.ID, "error", err)
	}
	h.drop.MarkCompleted(r.Context(), transfer.ID)
}
