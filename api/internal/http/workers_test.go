package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/config"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/scheduler"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/worker"
)

type stubExecutor struct{}

func (stubExecutor) Capacity(context.Context) (core.DockerCapacity, error) {
	return core.DockerCapacity{}, nil
}
func (stubExecutor) Create(context.Context, core.ExecutionRef, core.ContainerSpec) error { return nil }
func (stubExecutor) Start(context.Context, core.ExecutionRef) error                      { return nil }
func (stubExecutor) Inspect(context.Context, core.ExecutionRef) (core.ExecutionStatus, error) {
	return core.ExecutionStatus{}, nil
}
func (stubExecutor) Stop(context.Context, core.ExecutionRef) error           { return nil }
func (stubExecutor) Remove(context.Context, core.ExecutionRef) error         { return nil }
func (stubExecutor) Logs(context.Context, core.ExecutionRef) (string, error) { return "", nil }

type workerTestEnv struct {
	router      http.Handler
	idStore     *identity.Store
	coreStore   *core.Store
	workerStore *worker.Store
	engine      *core.Engine
	operator    string
}

func newWorkerTestEnv(t *testing.T) *workerTestEnv {
	t.Helper()
	ctx := context.Background()
	cfg := &config.Config{Token: "valid-token", Port: "8080"}
	coreStore, err := core.Open(ctx, filepath.Join(t.TempDir(), "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coreStore.Close() })
	idStore := identity.NewStore(coreStore.DB(), cfg.Token, "admin-password")
	if err := idStore.EnsureAdminBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	workerStore := worker.NewStore(coreStore.DB())
	workerStore.UseLocalStore(coreStore)
	engine := core.NewEngine(coreStore, stubExecutor{}, nil)
	engine.Cluster = workerStore
	if err := engine.Register(ctx); err != nil {
		t.Fatal(err)
	}
	engineCtx, cancelEngine := context.WithCancel(ctx)
	engineDone := make(chan struct{})
	go func() { defer close(engineDone); engine.Run(engineCtx) }()
	t.Cleanup(func() { cancelEngine(); <-engineDone })
	router, err := NewRouterWithServices(cfg, registry.NewRegistry(), scheduler.NewQueue(nil), engine, idStore, nil, workerStore)
	if err != nil {
		t.Fatal(err)
	}
	return &workerTestEnv{
		router: router, idStore: idStore, coreStore: coreStore, workerStore: workerStore,
		engine: engine, operator: "Bearer valid-token",
	}
}

func (env *workerTestEnv) do(t *testing.T, method, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec
}

func (env *workerTestEnv) session(t *testing.T, username string, kind string) string {
	t.Helper()
	ctx := context.Background()
	user, _, err := env.idStore.CreateUser(ctx, identity.LocalAdminID, identity.CreateUserInput{
		Username: username, DisplayName: username, Password: "correct-horse-battery",
	}, true)
	if err != nil {
		if !strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), "conflict") {
			t.Fatal(err)
		}
		user, err = env.idStore.GetUserByUsername(ctx, username)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, token, err := env.idStore.IssueSession(ctx, user.ID, kind, "test", "", identity.DefaultProject, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + token
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestWorkerEnrollmentAndApprovalLifecycle(t *testing.T) {
	env := newWorkerTestEnv(t)

	// Only administrators create enrollment credentials.
	if rec := env.do(t, "POST", "/v1/workers/enrollments", env.session(t, "developer1", "cli"), `{"name":"dev"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("member creating enrollment = %d", rec.Code)
	}
	rec := env.do(t, "POST", "/v1/workers/enrollments", env.operator, `{"name":"owner laptop","ttl_seconds":900,"auto_approve":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create enrollment = %d %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("enrollment token missing: %s", rec.Body.String())
	}

	// Enrollment without a valid token fails; with one it produces a pending worker.
	if rec := env.do(t, "POST", "/v1/workers/enroll", "", `{"token":"kwebogus","name":"laptop"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus enrollment = %d", rec.Code)
	}
	rec = env.do(t, "POST", "/v1/workers/enroll", "", `{"token":"`+token+`","name":"owner laptop","hostname":"laptop.local"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body.String())
	}
	enrolled := decodeBody(t, rec)
	workerID, _ := enrolled["worker_id"].(string)
	workerToken, _ := enrolled["worker_token"].(string)
	if workerID == "" || workerToken == "" || enrolled["approved"] == true {
		t.Fatalf("enrollment result = %s", rec.Body.String())
	}

	// The enrollment token is single-use.
	if rec := env.do(t, "POST", "/v1/workers/enroll", "", `{"token":"`+token+`","name":"again"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed enrollment = %d", rec.Code)
	}

	// A pending worker may heartbeat but receives no assignments.
	auth := "Bearer " + workerToken
	caps := `{"capabilities":{"os":"linux","arch":"arm64","logical_cpus":8,"memory_total_bytes":17179869184,"docker_available":true},"rtt_ms":35}`
	if rec := env.do(t, "POST", "/v1/workers/"+workerID+"/heartbeat", auth, caps); rec.Code != http.StatusOK {
		t.Fatalf("pending heartbeat = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, "POST", "/v1/workers/"+workerID+"/poll", auth, `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("pending poll = %d", rec.Code)
	}

	// Worker tokens are scoped: worker A cannot act as worker B.
	if rec := env.do(t, "POST", "/v1/workers/wk-other/heartbeat", auth, caps); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-worker heartbeat = %d", rec.Code)
	}

	// Approval enables assignments; member sessions cannot manage workers.
	if rec := env.do(t, "POST", "/v1/workers/"+workerID+"/approve", env.session(t, "developer2", "cli"), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("member approving worker = %d", rec.Code)
	}
	rec = env.do(t, "POST", "/v1/workers/"+workerID+"/approve", env.operator, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	approved := decodeBody(t, rec)
	if approved["state"] != "online" { // heartbeat arrived before approval
		t.Fatalf("state = %v", approved["state"])
	}
	if rec := env.do(t, "POST", "/v1/workers/"+workerID+"/poll", auth, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("approved poll = %d", rec.Code)
	}

	// Listing workers is administrative.
	if rec := env.do(t, "GET", "/v1/workers", env.session(t, "developer3", "cli"), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("member listing workers = %d", rec.Code)
	}
	rec = env.do(t, "GET", "/v1/workers", env.operator, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), workerID) {
		t.Fatalf("worker list = %d %s", rec.Code, rec.Body.String())
	}

	// Revocation immediately invalidates the worker token.
	rec = env.do(t, "POST", "/v1/workers/"+workerID+"/revoke", env.operator, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if rec := env.do(t, "POST", "/v1/workers/"+workerID+"/poll", auth, `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked poll = %d", rec.Code)
	}
	if rec := env.do(t, "POST", "/v1/workers/"+workerID+"/heartbeat", auth, caps); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked heartbeat = %d", rec.Code)
	}
}

func TestPairedDeviceSessionsCannotManageOrEnrollWorkers(t *testing.T) {
	env := newWorkerTestEnv(t)
	paired := env.session(t, "phoneuser", identity.SessionPairedDevice)

	if rec := env.do(t, "POST", "/v1/workers/enrollments", paired, `{"name":"evil"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("paired device creating enrollment = %d", rec.Code)
	}
	if rec := env.do(t, "GET", "/v1/workers", paired, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("paired device listing workers = %d", rec.Code)
	}
	if rec := env.do(t, "GET", "/v1/scheduler/status", paired, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("paired device scheduler status = %d", rec.Code)
	}
	// Enrollment itself requires a single-use kwe credential that pairing
	// never grants; the endpoint rejects unknown tokens.
	if rec := env.do(t, "POST", "/v1/workers/enroll", paired, `{"token":"kwenope","name":"phone"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("paired device enrollment = %d", rec.Code)
	}
}

func TestManualPlacementValidationAndExplanations(t *testing.T) {
	env := newWorkerTestEnv(t)

	// Create and approve a worker with real capabilities.
	_, token, _ := env.workerStore.CreateEnrollment(context.Background(), identity.LocalAdminID, "gpu-box", time.Minute, true)
	workerID, _, _, err := env.workerStore.Enroll(context.Background(), token, "gpu-box", "")
	if err != nil {
		t.Fatal(err)
	}
	cpus := 4
	total := int64(8 << 30)
	storage := int64(500 << 30)
	docker := true
	caps := &core.Capabilities{OS: "linux", Arch: "arm64", LogicalCPUs: &cpus, MemoryTotalBytes: &total, StorageAvailable: &storage, DockerAvailable: &docker}
	if err := env.workerStore.Heartbeat(context.Background(), workerID, caps, nil, 20); err != nil {
		t.Fatal(err)
	}
	if _, err := env.workerStore.SetStatus(context.Background(), workerID, core.WorkerStateApproved); err != nil {
		t.Fatal(err)
	}

	// Manual placement on a compatible worker is accepted.
	key := "manual-ok"
	body := `{"image":"example:local","placement":"manual","worker_id":"` + workerID + `"}`
	req := httptest.NewRequest("POST", "/v1/workloads", strings.NewReader(body))
	req.Header.Set("Authorization", env.operator)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("manual submit = %d %s", rec.Code, rec.Body.String())
	}
	var created core.Workload
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// Manual placement on an unknown worker is rejected with an explanation.
	bad := `{"image":"example:local","placement":"manual","worker_id":"wk-` + strings.Repeat("0", 32) + `"}`
	req = httptest.NewRequest("POST", "/v1/workloads", strings.NewReader(bad))
	req.Header.Set("Authorization", env.operator)
	req.Header.Set("Idempotency-Key", "manual-bad")
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown manual worker = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not registered") {
		t.Fatalf("explanation = %s", rec.Body.String())
	}

	// The workload explanation endpoint exposes placement decisions.
	var explanation []core.PlacementDecision
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec = env.do(t, "GET", "/v1/workloads/"+created.ID+"/explanation", env.operator, "")
		if rec.Code == http.StatusOK {
			_ = json.Unmarshal(rec.Body.Bytes(), &explanation)
			if len(explanation) > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			if w, err := env.coreStore.Get(context.Background(), created.ID); err == nil {
				t.Logf("workload state=%s reason=%q assignment=%v", w.State, w.QueueReason, w.Assignment != nil)
			} else {
				t.Logf("workload read error: %v", err)
			}
			if ws, err := env.workerStore.Workers(context.Background()); err == nil {
				for _, wk := range ws {
					t.Logf("worker %s state=%s health=%s runtime=%v", wk.ID, wk.State, wk.Health, wk.Runtime != nil)
				}
			}
			if all, err := env.workerStore.RecentDecisions(context.Background(), 20); err == nil {
				t.Logf("recent decisions: %d", len(all))
			} else {
				t.Logf("recent decisions error: %v", err)
			}
			t.Fatalf("no placement decision recorded: %d %s", rec.Code, rec.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if explanation[0].Mode != core.PlacementManual || explanation[0].Chosen != workerID {
		t.Fatalf("explanation = %+v", explanation[0])
	}

	// Cross-project isolation: another user's session cannot read it.
	outsider := env.session(t, "outsider", "cli")
	if rec := env.do(t, "GET", "/v1/workloads/"+created.ID+"/explanation", outsider, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider explanation = %d", rec.Code)
	}
	if rec := env.do(t, "GET", "/v1/workloads/"+created.ID, outsider, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider workload = %d", rec.Code)
	}
}

func TestSchedulingPreferencesEndpoint(t *testing.T) {
	env := newWorkerTestEnv(t)
	ctx := context.Background()
	if _, err := env.idStore.UpdateSettings(ctx, identity.LocalAdminID, identity.Settings{
		CloudMode: identity.CloudModeCollaborative, RegistrationPolicy: identity.RegistrationOwnerOnly,
	}); err != nil {
		t.Fatal(err)
	}
	memberUser, _, err := env.idStore.CreateUser(ctx, identity.LocalAdminID, identity.CreateUserInput{
		Username: "member1", DisplayName: "member1", Password: "correct-horse-battery",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.idStore.SetMemberRole(ctx, identity.LocalAdminID, "owner", "default", memberUser.ID, "developer"); err != nil {
		t.Fatal(err)
	}
	member := env.session(t, "member1", "cli")

	if rec := env.do(t, "GET", "/v1/projects/default/scheduling-preferences", member, ""); rec.Code != http.StatusOK {
		t.Fatalf("member reading prefs = %d", rec.Code)
	}
	if rec := env.do(t, "PUT", "/v1/projects/default/scheduling-preferences", member,
		`{"mode":"preferred","preferred_workers":["local"],"fallback":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("member writing prefs = %d", rec.Code)
	}
	rec := env.do(t, "PUT", "/v1/projects/default/scheduling-preferences", env.operator,
		`{"mode":"preferred","preferred_workers":["local"],"fallback":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner writing prefs = %d %s", rec.Code, rec.Body.String())
	}
	var prefs map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &prefs)
	if prefs["mode"] != "preferred" || prefs["fallback"] != false {
		t.Fatalf("prefs = %s", rec.Body.String())
	}
	if rec := env.do(t, "PUT", "/v1/projects/default/scheduling-preferences", env.operator,
		`{"mode":"manual"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("manual mapping without a worker = %d", rec.Code)
	}
}

func TestWorkerPolicyEndpoint(t *testing.T) {
	env := newWorkerTestEnv(t)
	_, token, err := env.workerStore.CreateEnrollment(context.Background(), identity.LocalAdminID, "policy", time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	workerID, workerToken, _, err := env.workerStore.Enroll(context.Background(), token, "policy-laptop", "")
	if err != nil {
		t.Fatal(err)
	}

	if rec := env.do(t, "PUT", "/v1/workers/"+workerID+"/policy", env.session(t, "member2", "cli"),
		`{"gpu_allowed":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("member policy update = %d", rec.Code)
	}
	rec := env.do(t, "PUT", "/v1/workers/"+workerID+"/policy", env.operator,
		`{"gpu_allowed":false,"cpu_millis_max":4000,"projects":["default"],"max_concurrent":2,"schedule":[{"days":[1,2],"start":"09:00","end":"17:00"}],"timezone":"UTC"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy update = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, "PUT", "/v1/workers/"+workerID+"/policy", env.operator, `{"timezone":"Mars/Olympus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy = %d", rec.Code)
	}
	if _, err := env.workerStore.AuthenticateWorker(context.Background(), workerToken); err != nil {
		t.Fatalf("policy update must not touch the token: %v", err)
	}
}
