package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/config"
)

func TestRouter(t *testing.T) {
	cfg := &config.Config{
		Token: "test-secret-token",
		Port:  "8080",
	}
	router := NewRouter(cfg)

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
			name:         "any other path without token returns 401",
			method:       http.MethodGet,
			path:         "/v1/nodes",
			authHeader:   "",
			wantStatus:   http.StatusUnauthorized,
			wantCode:     "unauthorized",
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
			name:         "wrong token returns 401",
			method:       http.MethodGet,
			path:         "/v1/nodes",
			authHeader:   "Bearer wrong-token",
			wantStatus:   http.StatusUnauthorized,
			wantCode:     "unauthorized",
			isErrorCheck: true,
		},
		{
			name:         "wrong scheme Basic returns 401",
			method:       http.MethodGet,
			path:         "/v1/nodes",
			authHeader:   "Basic test-secret-token",
			wantStatus:   http.StatusUnauthorized,
			wantCode:     "unauthorized",
			isErrorCheck: true,
		},
		{
			name:         "wrong scheme Token returns 401",
			method:       http.MethodGet,
			path:         "/v1/nodes",
			authHeader:   "Token test-secret-token",
			wantStatus:   http.StatusUnauthorized,
			wantCode:     "unauthorized",
			isErrorCheck: true,
		},
		{
			name:         "valid token on unknown path returns 404",
			method:       http.MethodGet,
			path:         "/unknown",
			authHeader:   "Bearer test-secret-token",
			wantStatus:   http.StatusNotFound,
			wantCode:     "not_found",
			isErrorCheck: true,
		},
		{
			name:         "wrong method on known route healthz returns 400 bad_request",
			method:       http.MethodPost,
			path:         "/healthz",
			authHeader:   "",
			wantStatus:   http.StatusBadRequest,
			wantCode:     "bad_request",
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
				var errResp errorResponse
				if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
					t.Fatalf("failed to decode error body: %v", err)
				}
				if errResp.Error.Code != tt.wantCode {
					t.Fatalf("expected code %q, got %q", tt.wantCode, errResp.Error.Code)
				}
				if errResp.Error.Message == "" {
					t.Fatalf("expected non-empty error message")
				}
			} else {
				trimmed := strings.TrimSpace(rec.Body.String())
				if trimmed != tt.wantBody {
					t.Fatalf("expected body %s, got %s", tt.wantBody, trimmed)
				}
			}
		})
	}
}
