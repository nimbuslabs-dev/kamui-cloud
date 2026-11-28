package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/runtime"
)

func TestServiceRouteRequiresLiveFencedAssignment(t *testing.T) {
	now := time.Now()
	route := runtime.ServiceRoute{WorkloadID: "test", ProjectID: "project", WorkerID: "worker", Attempt: 2, Fence: "current", ExpiresAt: now.Add(time.Minute)}
	w := core.Workload{ID: "test", ProjectID: "project", State: core.Running, Attempt: 2, Assignment: &core.Assignment{WorkerID: "worker", Fence: "current", LeaseUntil: now.Add(time.Minute)}}
	if !liveServiceRoute(route, w, now) {
		t.Fatal("current route rejected")
	}
	for _, alter := range []func(*runtime.ServiceRoute){
		func(r *runtime.ServiceRoute) { r.Fence = "stale" },
		func(r *runtime.ServiceRoute) { r.ProjectID = "other" },
		func(r *runtime.ServiceRoute) { r.Attempt-- },
		func(r *runtime.ServiceRoute) { r.ExpiresAt = now },
	} {
		stale := route
		alter(&stale)
		if liveServiceRoute(stale, w, now) {
			t.Fatal("stale route accepted")
		}
	}
	w.Assignment.LeaseUntil = now
	if liveServiceRoute(route, w, now) {
		t.Fatal("expired lease accepted")
	}
}

func TestServiceProxyStripsCoordinatorCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.URL.RequestURI() != "/v1/models?limit=2" {
			t.Errorf("upstream did not receive the expected sanitized request")
		}
		w.Header().Set("Set-Cookie", "upstream=1")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	r := httptest.NewRequest("GET", "https://localhost/v1/projects/default/inference/demo/proxy/v1/models?limit=2", nil)
	r.SetPathValue("path", "v1/models")
	r.Header.Set("Authorization", "Bearer test")
	r.Header.Set("Cookie", "session=test")
	w := httptest.NewRecorder()
	proxyService(w, r, upstream.Listener.Addr().String(), http.DefaultTransport)
	if w.Code != http.StatusAccepted || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("proxy response: %d %v", w.Code, w.Header())
	}
}
