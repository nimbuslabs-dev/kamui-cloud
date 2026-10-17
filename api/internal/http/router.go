package httpapi

import (
	"net/http"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/config"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
)

type responseInterceptor struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (rw *responseInterceptor) WriteHeader(status int) {
	if rw.wrote {
		return
	}
	rw.status = status
	if status == http.StatusMethodNotAllowed {
		return
	}
	if status == http.StatusNotFound && rw.Header().Get("Content-Type") != "application/json" {
		return
	}
	rw.wrote = true
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseInterceptor) Write(b []byte) (int, error) {
	if rw.status == http.StatusMethodNotAllowed {
		return len(b), nil
	}
	if rw.status == http.StatusNotFound && !rw.wrote {
		return len(b), nil
	}
	if !rw.wrote {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

func wrapMux(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		interceptor := &responseInterceptor{ResponseWriter: w}
		mux.ServeHTTP(interceptor, r)

		if interceptor.status == http.StatusMethodNotAllowed {
			interceptor.Header().Del("Allow")
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "method not allowed")
		} else if interceptor.status == http.StatusNotFound && !interceptor.wrote {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "not found")
		}
	})
}

func NewRouter(cfg *config.Config, reg ...*registry.Registry) http.Handler {
	var r *registry.Registry
	if len(reg) > 0 && reg[0] != nil {
		r = reg[0]
	} else {
		r = registry.NewRegistry()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", HandleHealthz)

	nodeHandler := NewNodeHandler(r)
	mux.HandleFunc("POST /v1/nodes/register", nodeHandler.Register)
	mux.HandleFunc("GET /v1/nodes", nodeHandler.List)
	mux.HandleFunc("POST /v1/nodes/{id}/heartbeat", nodeHandler.Heartbeat)
	mux.HandleFunc("DELETE /v1/nodes/{id}", nodeHandler.Delete)

	wrappedMux := wrapMux(mux)
	authenticated := auth.Middleware(cfg.Token)(wrappedMux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			wrappedMux.ServeHTTP(w, r)
			return
		}
		authenticated.ServeHTTP(w, r)
	})
}
