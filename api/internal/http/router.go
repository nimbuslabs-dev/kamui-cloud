package httpapi

import (
	"net/http"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/config"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/connect"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/runtime"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/scheduler"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/worker"
)

func NewRouter(cfg *config.Config, reg *registry.Registry, queue *scheduler.Queue) (http.Handler, error) {
	return NewRouterWithServices(cfg, reg, queue, nil, nil, nil, nil, nil)
}

func NewRouterWithCore(cfg *config.Config, reg *registry.Registry, queue *scheduler.Queue, engine *core.Engine) (http.Handler, error) {
	return NewRouterWithServices(cfg, reg, queue, engine, nil, nil, nil, nil)
}

func NewRouterWithIdentity(cfg *config.Config, reg *registry.Registry, queue *scheduler.Queue, engine *core.Engine, idStore *identity.Store, connStore *connect.Store) (http.Handler, error) {
	return NewRouterWithServices(cfg, reg, queue, engine, idStore, connStore, nil, nil)
}

func NewRouterWithServices(
	cfg *config.Config,
	reg *registry.Registry,
	queue *scheduler.Queue,
	engine *core.Engine,
	idStore *identity.Store,
	connStore *connect.Store,
	workerStore *worker.Store,
	runtimeStore *runtime.Store,
) (http.Handler, error) {
	mux := http.NewServeMux()
	if engine != nil {
		registerCoreWithIdentity(mux, engine, idStore, workerStore)
	}
	mux.HandleFunc("GET /healthz", HandleHealthz)

	var validator auth.TokenValidator
	if idStore != nil {
		validator = idStore
	}

	adminOnly := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if idStore != nil {
				p, ok := auth.FromContext(r.Context())
				if !ok {
					apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
					return
				}
				if !p.User.IsSystemAdmin {
					apierr.WriteError(w, http.StatusForbidden, "forbidden", "administrator permission required")
					return
				}
			}
			next(w, r)
		}
	}

	nodeHandler := NewNodeHandler(reg)
	mux.HandleFunc("POST /v1/nodes/register", adminOnly(nodeHandler.Register))
	mux.HandleFunc("GET /v1/nodes", adminOnly(nodeHandler.List))
	mux.HandleFunc("POST /v1/nodes/{id}/heartbeat", adminOnly(nodeHandler.Heartbeat))
	mux.HandleFunc("DELETE /v1/nodes/{id}", adminOnly(nodeHandler.Delete))

	jobHandler := NewJobHandler(reg, queue)
	mux.HandleFunc("POST /v1/jobs", adminOnly(jobHandler.Create))
	mux.HandleFunc("GET /v1/jobs", adminOnly(jobHandler.List))
	mux.HandleFunc("POST /v1/jobs/claim", adminOnly(jobHandler.Claim))
	mux.HandleFunc("GET /v1/jobs/{id}", adminOnly(jobHandler.Get))
	mux.HandleFunc("POST /v1/jobs/{id}/finish", adminOnly(jobHandler.Finish))

	workspaceHandler, err := newWorkspaceHandlerWithIdentity(cfg.WorkspaceDataFile, cfg.WorkspaceManagerURL, cfg.Token, idStore)
	if err != nil {
		return nil, err
	}
	mux.HandleFunc("POST /v1/workspaces", workspaceHandler.Create)
	mux.HandleFunc("GET /v1/workspaces", workspaceHandler.List)
	mux.HandleFunc("GET /v1/workspaces/{id}", workspaceHandler.Get)
	mux.HandleFunc("POST /v1/workspaces/{id}/start", workspaceHandler.Start)
	mux.HandleFunc("POST /v1/workspaces/{id}/stop", workspaceHandler.Stop)
	mux.HandleFunc("POST /v1/workspaces/{id}/restart", workspaceHandler.Restart)
	mux.HandleFunc("POST /v1/workspaces/{id}/git/branch", workspaceHandler.GitBranch)
	mux.HandleFunc("DELETE /v1/workspaces/{id}", workspaceHandler.Delete)
	mux.HandleFunc("GET /v1/workspaces/{id}/git/status", workspaceHandler.GitStatus)
	mux.HandleFunc("POST /v1/workspaces/{id}/git/add", workspaceHandler.GitAdd)
	mux.HandleFunc("POST /v1/workspaces/{id}/git/commit", workspaceHandler.GitCommit)
	mux.HandleFunc("POST /v1/workspaces/{id}/git/push", workspaceHandler.GitPush)
	mux.HandleFunc("POST /v1/workspaces/{id}/git/pull", workspaceHandler.GitPull)

	publicMux := http.NewServeMux()
	publicMux.HandleFunc("GET /healthz", HandleHealthz)

	if idStore != nil {
		ih := newIdentityHandler(idStore)
		publicMux.HandleFunc("POST /v1/auth/login", ih.Login)
		publicMux.HandleFunc("POST /v1/auth/register", ih.Register)
		publicMux.HandleFunc("POST /v1/invitations/accept", ih.AcceptInvitation)
		publicMux.HandleFunc("POST /v1/github/oauth/start", ih.GitHubOAuthStart)
		publicMux.HandleFunc("POST /v1/github/oauth/exchange", ih.GitHubOAuthExchange)

		mux.HandleFunc("POST /v1/auth/logout", ih.Logout)
		mux.HandleFunc("GET /v1/auth/me", ih.Me)
		mux.HandleFunc("POST /v1/auth/active-project", ih.SetActiveProject)
		mux.HandleFunc("GET /v1/auth/sessions", ih.ListSessions)
		mux.HandleFunc("DELETE /v1/auth/sessions/{id}", ih.RevokeSession)
		mux.HandleFunc("GET /v1/identity/settings", ih.GetSettings)
		mux.HandleFunc("PATCH /v1/identity/settings", ih.UpdateSettings)
		mux.HandleFunc("GET /v1/users", ih.ListUsers)
		mux.HandleFunc("POST /v1/users", ih.Register)
		mux.HandleFunc("POST /v1/users/{id}/disable", ih.SetUserDisabled(true))
		mux.HandleFunc("POST /v1/users/{id}/enable", ih.SetUserDisabled(false))
		mux.HandleFunc("GET /v1/projects", ih.ListProjects)
		mux.HandleFunc("POST /v1/projects", ih.CreateProject)
		mux.HandleFunc("GET /v1/projects/{id}", ih.GetProject)
		mux.HandleFunc("DELETE /v1/projects/{id}", ih.DeleteProject)
		mux.HandleFunc("GET /v1/projects/{id}/members", ih.ListMembers)
		mux.HandleFunc("POST /v1/projects/{id}/members", ih.SetMember)
		mux.HandleFunc("PATCH /v1/projects/{id}/members/{userId}", ih.SetMember)
		mux.HandleFunc("DELETE /v1/projects/{id}/members/{userId}", ih.RemoveMember)
		mux.HandleFunc("GET /v1/projects/{id}/invitations", ih.ListInvitations)
		mux.HandleFunc("POST /v1/projects/{id}/invitations", ih.CreateInvitation)
		mux.HandleFunc("DELETE /v1/projects/{id}/invitations/{inviteId}", ih.RevokeInvitation)
		mux.HandleFunc("GET /v1/projects/{id}/audit-events", ih.ListAuditEvents)
		mux.HandleFunc("GET /v1/audit-events", ih.ListAuditEvents)
		mux.HandleFunc("GET /v1/github/status", ih.GitHubStatus)
		mux.HandleFunc("PUT /v1/github/config", ih.SaveGitHubConfig)
		mux.HandleFunc("DELETE /v1/github/connection", ih.GitHubDisconnect)
		mux.HandleFunc("GET /v1/github/repos", ih.GitHubRepos)
	}

	if connStore != nil {
		ch := newConnectHandler(connStore, cfg.CLIGatewayPort, "https")
		publicMux.HandleFunc("POST /v1/connect/pairings/redeem", ch.RedeemPairing)

		mux.HandleFunc("GET /v1/connect/info", ch.Info)
		mux.HandleFunc("POST /v1/connect/pairings", ch.CreatePairing)
		mux.HandleFunc("DELETE /v1/connect/pairings/{id}", ch.RevokePairing)
		mux.HandleFunc("GET /v1/connect/devices", ch.ListDevices)
		mux.HandleFunc("POST /v1/connect/devices/{id}/approve", ch.ApproveDevice)
		mux.HandleFunc("POST /v1/connect/devices/{id}/revoke", ch.RevokeDevice)
		mux.HandleFunc("DELETE /v1/connect/devices/{id}", ch.RevokeDevice)
		mux.HandleFunc("GET /v1/connect/remote", ch.GetRemoteConfig)
		mux.HandleFunc("PUT /v1/connect/remote", ch.UpdateRemoteConfig)
		mux.HandleFunc("DELETE /v1/connect/remote", ch.DisableRemoteConfig)
	}

	if workerStore != nil {
		newWorkerHandler(workerStore, engine, idStore, runtimeStore).registerWorkerProtocol(publicMux, mux, adminOnly)
		if runtimeStore != nil {
			newRuntimeHandler(runtimeStore, engine, idStore).register(mux)
		}
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "not found")
	})

	authenticated := auth.SessionMiddleware(cfg.Token, validator)(mux)
	optionalAuth := auth.OptionalSessionMiddleware(cfg.Token, validator)(publicMux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := publicMux.Handler(r); pattern != "" {
			optionalAuth.ServeHTTP(w, r)
			return
		}
		authenticated.ServeHTTP(w, r)
	}), nil
}
