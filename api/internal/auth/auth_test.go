package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
)

func TestMiddleware(t *testing.T) {
	expectedToken := "correct-secret"
	middleware := Middleware(expectedToken)
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := middleware(dummyHandler)

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing authorization header",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "wrong scheme Basic",
			authHeader: "Basic correct-secret",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "wrong scheme Token",
			authHeader: "Token correct-secret",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "bearer prefix only without token",
			authHeader: "Bearer ",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "wrong token",
			authHeader: "Bearer wrong-secret",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "correct token",
			authHeader: "Bearer correct-secret",
			wantStatus: http.StatusOK,
			wantCode:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d", tt.wantStatus, rec.Code)
			}

			if tt.wantStatus == http.StatusUnauthorized {
				contentType := rec.Header().Get("Content-Type")
				if contentType != "application/json" {
					t.Fatalf("expected Content-Type application/json, got %q", contentType)
				}

				var body apierr.ErrorResponse
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}
				if body.Error.Code != tt.wantCode {
					t.Fatalf("expected code %q, got %q", tt.wantCode, body.Error.Code)
				}
				if body.Error.Message == "" {
					t.Fatalf("expected non-empty error message")
				}
			}
		})
	}
}
