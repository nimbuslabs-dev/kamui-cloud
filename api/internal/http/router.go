package httpapi

import (
	"net/http"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/config"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/scheduler"
)

func NewRouter(cfg *config.Config, reg *registry.Registry, queue *scheduler.Queue) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", HandleHealthz)

	nodeHandler := NewNodeHandler(reg)
	mux.HandleFunc("POST /v1/nodes/register", nodeHandler.Register)
	mux.HandleFunc("GET /v1/nodes", nodeHandler.List)
	mux.HandleFunc("POST /v1/nodes/{id}/heartbeat", nodeHandler.Heartbeat)
	mux.HandleFunc("DELETE /v1/nodes/{id}", nodeHandler.Delete)

	jobHandler := NewJobHandler(reg, queue)
	mux.HandleFunc("POST /v1/jobs", jobHandler.Create)
	mux.HandleFunc("GET /v1/jobs", jobHandler.List)
	mux.HandleFunc("POST /v1/jobs/claim", jobHandler.Claim)
	mux.HandleFunc("GET /v1/jobs/{id}", jobHandler.Get)
	mux.HandleFunc("POST /v1/jobs/{id}/finish", jobHandler.Finish)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "not found")
	})

	authenticated := auth.Middleware(cfg.Token)(mux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		authenticated.ServeHTTP(w, r)
	})
}
