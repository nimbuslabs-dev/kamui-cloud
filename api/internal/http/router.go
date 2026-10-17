package httpapi

import (
	"net/http"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/config"
)

func NewRouter(cfg *config.Config) http.Handler {
	authenticated := auth.Middleware(cfg.Token)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusNotFound, "not_found", "not found")
	}))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			HandleHealthz(w, r)
			return
		}
		authenticated.ServeHTTP(w, r)
	})
}
