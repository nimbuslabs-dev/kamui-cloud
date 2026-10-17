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
)

func TestRouterBaseEndpoints(t *testing.T) {
	cfg := &config.Config{
		Token: "test-secret-token",
		Port:  "8080",
	}
	reg := registry.NewRegistry()
	router := NewRouter(cfg, reg)

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
	router := NewRouter(cfg, reg)

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

func TestNodeLifecycleAndValidation(t *testing.T) {
	cfg := &config.Config{Token: "valid-token", Port: "8080"}
	reg := registry.NewRegistry()
	currentTime := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	reg.Now = func() time.Time { return currentTime }

	router := NewRouter(cfg, reg)
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
