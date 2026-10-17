package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/config"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/scheduler"
)

func TestRouterBaseEndpoints(t *testing.T) {
	cfg := &config.Config{
		Token: "test-secret-token",
		Port:  "8080",
	}
	reg := registry.NewRegistry()
	queue := scheduler.NewQueue(nil)
	router := NewRouter(cfg, reg, queue)

	tests := []struct {
		name         string
		method       string
		path         string
		authHeader   string
		wantStatus   int
		wantBody     string
		wantCode     string
		isErrorCheck bool
	}{
		{
			name:         "healthz without token returns 200",
			method:       http.MethodGet,
			path:         "/healthz",
			authHeader:   "",
			wantStatus:   http.StatusOK,
			wantBody:     `{"status":"ok","service":"api","version":"0.1.0"}`,
			isErrorCheck: false,
		},
		{
			name:         "wrong method on healthz without token returns 401",
			method:       http.MethodPost,
			path:         "/healthz",
			authHeader:   "",
			wantStatus:   http.StatusUnauthorized,
			wantCode:     "unauthorized",
			isErrorCheck: true,
		},
		{
			name:         "wrong method on healthz with valid token returns 404 not_found",
			method:       http.MethodPost,
			path:         "/healthz",
			authHeader:   "Bearer test-secret-token",
			wantStatus:   http.StatusNotFound,
			wantCode:     "not_found",
			isErrorCheck: true,
		},
		{
			name:         "root path without token returns 401",
			method:       http.MethodGet,
			path:         "/",
			authHeader:   "",
			wantStatus:   http.StatusUnauthorized,
			wantCode:     "unauthorized",
			isErrorCheck: true,
		},
		{
			name:         "unknown path without token returns 401",
			method:       http.MethodGet,
			path:         "/unknown",
			authHeader:   "",
			wantStatus:   http.StatusUnauthorized,
			wantCode:     "unauthorized",
			isErrorCheck: true,
		},
		{
			name:         "unknown path with valid token returns 404",
			method:       http.MethodGet,
			path:         "/unknown",
			authHeader:   "Bearer test-secret-token",
			wantStatus:   http.StatusNotFound,
			wantCode:     "not_found",
			isErrorCheck: true,
		},
		{
			name:         "wrong token returns 401",
			method:       http.MethodGet,
			path:         "/v1/nodes",
			authHeader:   "Bearer wrong-token",
			wantStatus:   http.StatusUnauthorized,
			wantCode:     "unauthorized",
			isErrorCheck: true,
		},
		{
			name:         "wrong scheme returns 401",
			method:       http.MethodGet,
			path:         "/v1/nodes",
			authHeader:   "Basic test-secret-token",
			wantStatus:   http.StatusUnauthorized,
			wantCode:     "unauthorized",
			isErrorCheck: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d", tt.wantStatus, rec.Code)
			}

			contentType := rec.Header().Get("Content-Type")
			if contentType != "application/json" {
				t.Fatalf("expected Content-Type application/json, got %q", contentType)
			}

			if tt.isErrorCheck {
				var errResp apierr.ErrorResponse
				if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
					t.Fatalf("failed to decode error body: %v", err)
				}
				if errResp.Error.Code != tt.wantCode {
					t.Fatalf("expected code %q, got %q", tt.wantCode, errResp.Error.Code)
				}
			} else if tt.wantBody != "" {
				trimmed := strings.TrimSpace(rec.Body.String())
				if trimmed != tt.wantBody {
					t.Fatalf("expected body %s, got %s", tt.wantBody, trimmed)
				}
			}
		})
	}
}

func TestNodeEndpointsUnauthorized(t *testing.T) {
	cfg := &config.Config{Token: "test-token", Port: "8080"}
	reg := registry.NewRegistry()
	queue := scheduler.NewQueue(nil)
	router := NewRouter(cfg, reg, queue)

	endpoints := []struct {
		name   string
		method string
		path   string
	}{
		{name: "register without token", method: http.MethodPost, path: "/v1/nodes/register"},
		{name: "list without token", method: http.MethodGet, path: "/v1/nodes"},
		{name: "heartbeat without token", method: http.MethodPost, path: "/v1/nodes/n-1234/heartbeat"},
		{name: "delete without token", method: http.MethodDelete, path: "/v1/nodes/n-1234"},
	}

	for _, tt := range endpoints {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected status 401, got %d", rec.Code)
			}

			var errResp apierr.ErrorResponse
			if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
				t.Fatalf("failed to decode error: %v", err)
			}
			if errResp.Error.Code != "unauthorized" {
				t.Fatalf("expected code unauthorized, got %q", errResp.Error.Code)
			}
		})
	}
}

func TestJobEndpointsUnauthorized(t *testing.T) {
	cfg := &config.Config{Token: "test-token", Port: "8080"}
	reg := registry.NewRegistry()
	queue := scheduler.NewQueue(nil)
	router := NewRouter(cfg, reg, queue)

	endpoints := []struct {
		name   string
		method string
		path   string
	}{
		{name: "create job without token", method: http.MethodPost, path: "/v1/jobs"},
		{name: "list jobs without token", method: http.MethodGet, path: "/v1/jobs"},
		{name: "claim job without token", method: http.MethodPost, path: "/v1/jobs/claim"},
		{name: "get job without token", method: http.MethodGet, path: "/v1/jobs/j-1234"},
		{name: "finish job without token", method: http.MethodPost, path: "/v1/jobs/j-1234/finish"},
	}

	for _, tt := range endpoints {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected status 401, got %d", rec.Code)
			}

			var errResp apierr.ErrorResponse
			if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
				t.Fatalf("failed to decode error: %v", err)
			}
			if errResp.Error.Code != "unauthorized" {
				t.Fatalf("expected code unauthorized, got %q", errResp.Error.Code)
			}
		})
	}
}

func TestNodeLifecycleAndValidation(t *testing.T) {
	cfg := &config.Config{Token: "valid-token", Port: "8080"}
	reg := registry.NewRegistry()
	currentTime := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	reg.Now = func() time.Time { return currentTime }

	queue := scheduler.NewQueue(nil)
	router := NewRouter(cfg, reg, queue)
	authHeader := "Bearer valid-token"

	t.Run("empty list returns []", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		trimmed := strings.TrimSpace(rec.Body.String())
		if trimmed != "[]" {
			t.Fatalf("expected empty array [], got %q", trimmed)
		}
	})

	t.Run("register invalid os returns 400", func(t *testing.T) {
		payload := `{"hostname":"host1","os":"freebsd","arch":"amd64","cpu_cores":4,"ram_mb":4096,"ip":"10.0.0.1"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/register", strings.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(rec.Body).Decode(&errResp)
		if errResp.Error.Code != "bad_request" {
			t.Fatalf("expected bad_request, got %q", errResp.Error.Code)
		}
	})

	t.Run("register unknown JSON field returns 400", func(t *testing.T) {
		payload := `{"hostname":"host1","os":"linux","arch":"amd64","cpu_cores":4,"ram_mb":4096,"ip":"10.0.0.1","extra_field":"disallowed"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/register", strings.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(rec.Body).Decode(&errResp)
		if errResp.Error.Code != "bad_request" {
			t.Fatalf("expected bad_request, got %q", errResp.Error.Code)
		}
	})

	t.Run("register oversized body returns 400", func(t *testing.T) {
		oversizedPayload := fmt.Sprintf(`{"hostname":"%s","os":"linux","arch":"amd64","cpu_cores":4,"ram_mb":4096,"ip":"10.0.0.1"}`, strings.Repeat("a", 70000))
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/register", strings.NewReader(oversizedPayload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("register trailing garbage returns 400", func(t *testing.T) {
		payload := `{"hostname":"worker-trailing","os":"linux","arch":"amd64","cpu_cores":4,"ram_mb":4096,"ip":"10.0.0.5"} trailing_junk`
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/register", strings.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(rec.Body).Decode(&errResp)
		if errResp.Error.Code != "bad_request" {
			t.Fatalf("expected bad_request, got %q", errResp.Error.Code)
		}
	})

	var nodeID string
	t.Run("register valid node returns 201", func(t *testing.T) {
		payload := `{"hostname":"worker-1","os":"macos","arch":"arm64","cpu_cores":8,"ram_mb":16384,"ip":"192.168.1.10","roles":["run","infer"]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/register", strings.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d", rec.Code)
		}

		var regResp RegistrationResponse
		if err := json.NewDecoder(rec.Body).Decode(&regResp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if !strings.HasPrefix(regResp.ID, "n-") || len(regResp.ID) != 6 {
			t.Fatalf("invalid id format: %q", regResp.ID)
		}
		if regResp.HeartbeatIntervalS != 5 || regResp.OfflineAfterS != 15 {
			t.Fatalf("unexpected interval or offline values: %+v", regResp)
		}
		nodeID = regResp.ID
	})

	t.Run("registering twice with the same body returns 201 with the same id and one node in list", func(t *testing.T) {
		payload := `{"hostname":"worker-1","os":"macos","arch":"arm64","cpu_cores":8,"ram_mb":16384,"ip":"192.168.1.10","roles":["run","infer"]}`

		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/register", strings.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201 on second register, got %d", rec.Code)
		}

		var regResp RegistrationResponse
		if err := json.NewDecoder(rec.Body).Decode(&regResp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if regResp.ID != nodeID {
			t.Fatalf("expected same id %q on duplicate register, got %q", nodeID, regResp.ID)
		}

		reqList := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
		reqList.Header.Set("Authorization", authHeader)
		recList := httptest.NewRecorder()
		router.ServeHTTP(recList, reqList)

		var list []registry.Node
		if err := json.NewDecoder(recList.Body).Decode(&list); err != nil {
			t.Fatalf("failed to decode list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("expected exactly 1 node in list, got %d", len(list))
		}
	})

	t.Run("list contains registered node", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var list []registry.Node
		if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
			t.Fatalf("failed to decode list: %v", err)
		}
		if len(list) != 1 || list[0].ID != nodeID {
			t.Fatalf("expected node in list, got %+v", list)
		}
		if list[0].Status != "online" {
			t.Fatalf("expected online status, got %q", list[0].Status)
		}
	})

	t.Run("heartbeat bad load returns 400", func(t *testing.T) {
		badPayload := `{"cpu_pct":120}`
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/nodes/%s/heartbeat", nodeID), strings.NewReader(badPayload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("heartbeat valid load returns 204", func(t *testing.T) {
		loadPayload := `{"cpu_pct":45.5,"mem_pct":60.0}`
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/nodes/%s/heartbeat", nodeID), strings.NewReader(loadPayload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("expected empty body on 204")
		}
	})

	t.Run("heartbeat empty body returns 204", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/nodes/%s/heartbeat", nodeID), nil)
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rec.Code)
		}
	})

	t.Run("delete node returns 204", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/v1/nodes/%s", nodeID), nil)
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rec.Code)
		}
	})

	t.Run("heartbeat after delete returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/nodes/%s/heartbeat", nodeID), nil)
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(rec.Body).Decode(&errResp)
		if errResp.Error.Code != "not_found" {
			t.Fatalf("expected code not_found, got %q", errResp.Error.Code)
		}
	})

	t.Run("delete non-existent node returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/v1/nodes/%s", nodeID), nil)
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("wrong method on known routes returns 404 not_found", func(t *testing.T) {
		wrongMethods := []struct {
			method string
			path   string
		}{
			{method: http.MethodPut, path: "/v1/nodes"},
			{method: http.MethodGet, path: "/v1/nodes/register"},
			{method: http.MethodGet, path: "/v1/nodes/n-1234/heartbeat"},
			{method: http.MethodPost, path: "/v1/nodes/n-1234"},
			{method: http.MethodPost, path: "/healthz"},
		}

		for _, wm := range wrongMethods {
			req := httptest.NewRequest(wm.method, wm.path, nil)
			req.Header.Set("Authorization", authHeader)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404 for %s %s, got %d", wm.method, wm.path, rec.Code)
			}
			var errResp apierr.ErrorResponse
			_ = json.NewDecoder(rec.Body).Decode(&errResp)
			if errResp.Error.Code != "not_found" {
				t.Fatalf("expected not_found, got %q", errResp.Error.Code)
			}
		}
	})
}

func TestJobLifecycleAndValidation(t *testing.T) {
	cfg := &config.Config{Token: "valid-token", Port: "8080"}
	reg := registry.NewRegistry()
	currentTime := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	queue := scheduler.NewQueue(func() time.Time { return currentTime })
	router := NewRouter(cfg, reg, queue)
	authHeader := "Bearer valid-token"

	nodeID := reg.Register(registry.NodeInfo{
		Hostname: "worker-node",
		OS:       "linux",
		Arch:     "amd64",
		CPUCores: 4,
		RAMMB:    8192,
		IP:       "10.0.0.1",
	})

	t.Run("empty jobs list returns []", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		trimmed := strings.TrimSpace(rec.Body.String())
		if trimmed != "[]" {
			t.Fatalf("expected empty array [], got %q", trimmed)
		}
	})

	t.Run("image with space returns 400", func(t *testing.T) {
		payload := `{"image":"alpine 3.20"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(rec.Body).Decode(&errResp)
		if errResp.Error.Code != "bad_request" {
			t.Fatalf("expected bad_request, got %q", errResp.Error.Code)
		}
	})

	t.Run("empty claim returns 204 with no body", func(t *testing.T) {
		payload := fmt.Sprintf(`{"node_id":"%s"}`, nodeID)
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs/claim", strings.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("expected empty body on 204, got %s", rec.Body.String())
		}
	})

	t.Run("claim with unknown node_id is 400", func(t *testing.T) {
		payload := `{"node_id":"n-unknown"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs/claim", strings.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(rec.Body).Decode(&errResp)
		if errResp.Error.Code != "bad_request" {
			t.Fatalf("expected bad_request, got %q", errResp.Error.Code)
		}
	})

	t.Run("claim with missing node_id is 400", func(t *testing.T) {
		payload := `{}`
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs/claim", strings.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	var jobID string
	t.Run("create then claim then finish then get returns the logs", func(t *testing.T) {
		createPayload := `{"image":"alpine:3.20","command":["echo","hello"]}`
		reqCreate := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(createPayload))
		reqCreate.Header.Set("Authorization", authHeader)
		recCreate := httptest.NewRecorder()
		router.ServeHTTP(recCreate, reqCreate)

		if recCreate.Code != http.StatusCreated {
			t.Fatalf("expected 201 on job create, got %d", recCreate.Code)
		}
		var createdJob scheduler.Job
		if err := json.NewDecoder(recCreate.Body).Decode(&createdJob); err != nil {
			t.Fatalf("failed to decode created job: %v", err)
		}
		if createdJob.ID == "" || createdJob.Status != scheduler.StatusQueued {
			t.Fatalf("unexpected created job: %+v", createdJob)
		}
		jobID = createdJob.ID

		claimPayload := fmt.Sprintf(`{"node_id":"%s"}`, nodeID)
		reqClaim := httptest.NewRequest(http.MethodPost, "/v1/jobs/claim", strings.NewReader(claimPayload))
		reqClaim.Header.Set("Authorization", authHeader)
		recClaim := httptest.NewRecorder()
		router.ServeHTTP(recClaim, reqClaim)

		if recClaim.Code != http.StatusOK {
			t.Fatalf("expected 200 on job claim, got %d", recClaim.Code)
		}
		var claimedJob scheduler.Job
		if err := json.NewDecoder(recClaim.Body).Decode(&claimedJob); err != nil {
			t.Fatalf("failed to decode claimed job: %v", err)
		}
		if claimedJob.ID != jobID || claimedJob.Status != scheduler.StatusRunning || claimedJob.NodeID != nodeID {
			t.Fatalf("unexpected claimed job: %+v", claimedJob)
		}

		finishPayload := `{"exit_code":0,"logs":"execution completed successfully"}`
		reqFinish := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/jobs/%s/finish", jobID), strings.NewReader(finishPayload))
		reqFinish.Header.Set("Authorization", authHeader)
		recFinish := httptest.NewRecorder()
		router.ServeHTTP(recFinish, reqFinish)

		if recFinish.Code != http.StatusOK {
			t.Fatalf("expected 200 on job finish, got %d", recFinish.Code)
		}
		var finishedJob scheduler.Job
		if err := json.NewDecoder(recFinish.Body).Decode(&finishedJob); err != nil {
			t.Fatalf("failed to decode finished job: %v", err)
		}
		if finishedJob.Status != scheduler.StatusSucceeded || finishedJob.ExitCode == nil || *finishedJob.ExitCode != 0 {
			t.Fatalf("unexpected finished job: %+v", finishedJob)
		}

		reqGet := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/jobs/%s", jobID), nil)
		reqGet.Header.Set("Authorization", authHeader)
		recGet := httptest.NewRecorder()
		router.ServeHTTP(recGet, reqGet)

		if recGet.Code != http.StatusOK {
			t.Fatalf("expected 200 on job get, got %d", recGet.Code)
		}
		var fetchedJob scheduler.Job
		if err := json.NewDecoder(recGet.Body).Decode(&fetchedJob); err != nil {
			t.Fatalf("failed to decode fetched job: %v", err)
		}
		if fetchedJob.Logs != "execution completed successfully" {
			t.Fatalf("expected logs returned, got %q", fetchedJob.Logs)
		}
	})

	t.Run("finish with missing exit_code is 400", func(t *testing.T) {
		createPayload := `{"image":"alpine:3.20"}`
		reqCreate := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(createPayload))
		reqCreate.Header.Set("Authorization", authHeader)
		recCreate := httptest.NewRecorder()
		router.ServeHTTP(recCreate, reqCreate)
		var j scheduler.Job
		_ = json.NewDecoder(recCreate.Body).Decode(&j)

		claimPayload := fmt.Sprintf(`{"node_id":"%s"}`, nodeID)
		reqClaim := httptest.NewRequest(http.MethodPost, "/v1/jobs/claim", strings.NewReader(claimPayload))
		reqClaim.Header.Set("Authorization", authHeader)
		recClaim := httptest.NewRecorder()
		router.ServeHTTP(recClaim, reqClaim)

		finishPayload := `{"logs":"no exit code"}`
		reqFinish := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/jobs/%s/finish", j.ID), strings.NewReader(finishPayload))
		reqFinish.Header.Set("Authorization", authHeader)
		recFinish := httptest.NewRecorder()
		router.ServeHTTP(recFinish, reqFinish)

		if recFinish.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", recFinish.Code)
		}
	})

	t.Run("logs of 65537 characters is 400", func(t *testing.T) {
		createPayload := `{"image":"alpine:3.20"}`
		reqCreate := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(createPayload))
		reqCreate.Header.Set("Authorization", authHeader)
		recCreate := httptest.NewRecorder()
		router.ServeHTTP(recCreate, reqCreate)
		var j scheduler.Job
		_ = json.NewDecoder(recCreate.Body).Decode(&j)

		claimPayload := fmt.Sprintf(`{"node_id":"%s"}`, nodeID)
		reqClaim := httptest.NewRequest(http.MethodPost, "/v1/jobs/claim", strings.NewReader(claimPayload))
		reqClaim.Header.Set("Authorization", authHeader)
		recClaim := httptest.NewRecorder()
		router.ServeHTTP(recClaim, reqClaim)

		oversizedLogs := strings.Repeat("a", 65537)
		finishPayload := fmt.Sprintf(`{"exit_code":0,"logs":"%s"}`, oversizedLogs)
		reqFinish := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/jobs/%s/finish", j.ID), strings.NewReader(finishPayload))
		reqFinish.Header.Set("Authorization", authHeader)
		recFinish := httptest.NewRecorder()
		router.ServeHTTP(recFinish, reqFinish)

		if recFinish.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", recFinish.Code)
		}
	})

	t.Run("finish not running job returns 409", func(t *testing.T) {
		createPayload := `{"image":"alpine:3.20"}`
		reqCreate := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(createPayload))
		reqCreate.Header.Set("Authorization", authHeader)
		recCreate := httptest.NewRecorder()
		router.ServeHTTP(recCreate, reqCreate)
		var j scheduler.Job
		_ = json.NewDecoder(recCreate.Body).Decode(&j)

		finishPayload := `{"exit_code":0}`
		reqFinish := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/jobs/%s/finish", j.ID), strings.NewReader(finishPayload))
		reqFinish.Header.Set("Authorization", authHeader)
		recFinish := httptest.NewRecorder()
		router.ServeHTTP(recFinish, reqFinish)

		if recFinish.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d", recFinish.Code)
		}
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(recFinish.Body).Decode(&errResp)
		if errResp.Error.Code != "conflict" {
			t.Fatalf("expected conflict, got %q", errResp.Error.Code)
		}
	})

	t.Run("get unknown job returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/jobs/j-unknown", nil)
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	t.Run("finish unknown job returns 404", func(t *testing.T) {
		finishPayload := `{"exit_code":0}`
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs/j-unknown/finish", strings.NewReader(finishPayload))
		req.Header.Set("Authorization", authHeader)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})
}
