package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

type IdentityHandler struct {
	store      *identity.Store
	httpClient *http.Client
}

func newIdentityHandler(store *identity.Store) *IdentityHandler {
	return &IdentityHandler{
		store: store,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func writeIdentityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrUnauthorized):
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", err.Error())
	case errors.Is(err, identity.ErrForbidden):
		apierr.WriteError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, identity.ErrPersonalMode):
		apierr.WriteError(w, http.StatusForbidden, "personal_mode", "Switch cloud mode to collaborative to invite or manage team members.")
	case errors.Is(err, identity.ErrRegistrationOff):
		apierr.WriteError(w, http.StatusForbidden, "registration_restricted", err.Error())
	case errors.Is(err, identity.ErrNotFound):
		apierr.WriteError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, identity.ErrConflict):
		apierr.WriteError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, identity.ErrInviteExpired):
		apierr.WriteError(w, http.StatusGone, "invite_expired", err.Error())
	case errors.Is(err, identity.ErrInviteUsed):
		apierr.WriteError(w, http.StatusConflict, "invite_used", err.Error())
	case errors.Is(err, identity.ErrInviteRevoked):
		apierr.WriteError(w, http.StatusForbidden, "invite_revoked", err.Error())
	case errors.Is(err, identity.ErrRateLimited):
		apierr.WriteError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.Is(err, identity.ErrGitHubNotReady):
		apierr.WriteError(w, http.StatusServiceUnavailable, "github_not_configured", err.Error())
	case errors.Is(err, identity.ErrGitHubNotLinked):
		apierr.WriteError(w, http.StatusPreconditionFailed, "github_not_connected", err.Error())
	case errors.Is(err, identity.ErrBadInput):
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "identity operation failed")
	}
}

func requirePrincipal(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return identity.Principal{}, false
	}
	return p, true
}

func requireSystemAdmin(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return identity.Principal{}, false
	}
	if !p.User.IsSystemAdmin {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "system administrator permission required")
		return identity.Principal{}, false
	}
	return p, true
}

func (h *IdentityHandler) Login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		Kind       string `json:"kind"`
		Label      string `json:"label"`
		ProjectID  string `json:"project_id"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := readJSON(w, r, 16*1024, &in); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid login payload")
		return
	}
	if in.Password == "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "password is required")
		return
	}
	rateKey := r.RemoteAddr + "|" + strings.ToLower(strings.TrimSpace(in.Username))
	user, err := h.store.AuthenticateCredentials(r.Context(), in.Username, in.Password, rateKey)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	kind := in.Kind
	if kind == "" {
		kind = identity.SessionCLI
	}
	ttl := 24 * time.Hour
	if in.TTLSeconds > 0 && in.TTLSeconds <= 30*86400 {
		ttl = time.Duration(in.TTLSeconds) * time.Second
	}
	if in.ProjectID != "" {
		role, _ := h.store.GetMemberRole(r.Context(), in.ProjectID, user.ID)
		if role == "" {
			apierr.WriteError(w, http.StatusForbidden, "forbidden", "not a member of the requested project")
			return
		}
	}
	sess, token, err := h.store.IssueSession(r.Context(), user.ID, kind, in.Label, "", in.ProjectID, ttl)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	projects, _ := h.store.ListProjectsForUser(r.Context(), user.ID)
	settings, _ := h.store.GetSettings(r.Context())
	apierr.WriteJSON(w, http.StatusOK, map[string]any{
		"session_token": token,
		"session":       sess,
		"user":          user,
		"cloud_mode":    settings.CloudMode,
		"projects":      projects,
	})
}

func (h *IdentityHandler) Logout(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		All bool `json:"all"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(w, r, 4*1024, &in); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid logout payload")
			return
		}
	}
	if in.All {
		if err := h.store.RevokeAllUserSessions(r.Context(), p.User.ID); err != nil {
			writeIdentityError(w, err)
			return
		}
	} else if p.SessionID != "" {
		if err := h.store.RevokeSessionByID(r.Context(), p.User.ID, p.SessionID); err != nil && !errors.Is(err, identity.ErrNotFound) {
			writeIdentityError(w, err)
			return
		}
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (h *IdentityHandler) Register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username              string `json:"username"`
		Email                 string `json:"email"`
		DisplayName           string `json:"display_name"`
		Password              string `json:"password"`
		CreatePersonalProject *bool  `json:"create_personal_project"`
	}
	if err := readJSON(w, r, 16*1024, &in); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid registration payload")
		return
	}
	settings, err := h.store.GetSettings(r.Context())
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	caller, hasCaller := auth.FromContext(r.Context())
	actorID := ""
	if hasCaller {
		actorID = caller.User.ID
	}
	if settings.RegistrationPolicy == identity.RegistrationDisabled {
		writeIdentityError(w, identity.ErrRegistrationOff)
		return
	}
	if settings.RegistrationPolicy != identity.RegistrationOpen {
		if !hasCaller || !caller.User.IsSystemAdmin {
			writeIdentityError(w, identity.ErrRegistrationOff)
			return
		}
	}
	createProj := true
	if in.CreatePersonalProject != nil {
		createProj = *in.CreatePersonalProject
	}
	user, proj, err := h.store.CreateUser(r.Context(), actorID, identity.CreateUserInput{
		Username: in.Username, Email: in.Email, DisplayName: in.DisplayName, Password: in.Password,
	}, createProj)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, map[string]any{
		"user":             user,
		"personal_project": proj,
	})
}

func (h *IdentityHandler) Me(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	user, err := h.store.GetUser(r.Context(), p.User.ID)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	settings, err := h.store.GetSettings(r.Context())
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	projects, err := h.store.ListProjectsForUser(r.Context(), p.User.ID)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	gh, _ := h.store.GetGitHubStatus(r.Context(), p.User.ID)
	activeProject := p.ActiveProjectID
	var activeRole string
	var permitted []string
	for _, prj := range projects {
		if prj.ID == activeProject {
			activeRole = prj.Role
			permitted = prj.PermittedOperations
			break
		}
	}
	if activeRole == "" && len(projects) > 0 {
		activeProject = projects[0].ID
		activeRole = projects[0].Role
		permitted = projects[0].PermittedOperations
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{
		"user":                 user,
		"auth_method":          p.AuthMethod,
		"session_id":           p.SessionID,
		"device_id":            p.DeviceID,
		"cloud_mode":           settings.CloudMode,
		"registration_policy":  settings.RegistrationPolicy,
		"active_project_id":    activeProject,
		"active_role":          activeRole,
		"permitted_operations": permitted,
		"projects":             projects,
		"github":               gh,
	})
}

func (h *IdentityHandler) SetActiveProject(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		ProjectID string `json:"project_id"`
	}
	if err := readJSON(w, r, 4*1024, &in); err != nil || in.ProjectID == "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "project_id is required")
		return
	}
	if p.SessionID == "" {
		if _, err := h.store.Authorize(r.Context(), p, in.ProjectID, identity.PermProjectRead); err != nil {
			writeIdentityError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, map[string]string{"active_project_id": in.ProjectID})
		return
	}
	if err := h.store.SetActiveProject(r.Context(), p.SessionID, p.User.ID, in.ProjectID); err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]string{"active_project_id": in.ProjectID})
}

func (h *IdentityHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	sessions, err := h.store.ListUserSessions(r.Context(), p.User.ID)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, sessions)
}

func (h *IdentityHandler) RevokeSession(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	sid := r.PathValue("id")
	if !p.User.IsSystemAdmin {
		sessions, err := h.store.ListUserSessions(r.Context(), p.User.ID)
		if err != nil {
			writeIdentityError(w, err)
			return
		}
		owned := false
		for _, s := range sessions {
			if s.ID == sid {
				owned = true
				break
			}
		}
		if !owned {
			apierr.WriteError(w, http.StatusForbidden, "forbidden", "cannot revoke another user's session")
			return
		}
	}
	if err := h.store.RevokeSessionByID(r.Context(), p.User.ID, sid); err != nil {
		writeIdentityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *IdentityHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePrincipal(w, r); !ok {
		return
	}
	s, err := h.store.GetSettings(r.Context())
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, s)
}

func (h *IdentityHandler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := requireSystemAdmin(w, r)
	if !ok {
		return
	}
	var in identity.Settings
	if err := readJSON(w, r, 4*1024, &in); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid settings payload")
		return
	}
	updated, err := h.store.UpdateSettings(r.Context(), p.User.ID, in)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, updated)
}

func (h *IdentityHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireSystemAdmin(w, r); !ok {
		return
	}
	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, users)
}

func (h *IdentityHandler) SetUserDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := requireSystemAdmin(w, r)
		if !ok {
			return
		}
		u, err := h.store.SetUserDisabled(r.Context(), p.User.ID, r.PathValue("id"), disabled)
		if err != nil {
			writeIdentityError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, u)
	}
}

func (h *IdentityHandler) ListProjects(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	projects, err := h.store.ListProjectsForUser(r.Context(), p.User.ID)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, projects)
}

func (h *IdentityHandler) CreateProject(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		IsPersonal  bool   `json:"is_personal"`
	}
	if err := readJSON(w, r, 8*1024, &in); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid project payload")
		return
	}
	pm, err := h.store.CreateProject(r.Context(), p.User.ID, in.Name, in.Description, in.IsPersonal)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, pm)
}

func (h *IdentityHandler) GetProject(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	pm, err := h.store.GetProjectForUser(r.Context(), r.PathValue("id"), p.User.ID)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, pm)
}

func (h *IdentityHandler) DeleteProject(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	if _, err := h.store.Authorize(r.Context(), p, pid, identity.PermProjectDelete); err != nil {
		writeIdentityError(w, err)
		return
	}
	if err := h.store.DeleteProject(r.Context(), p.User.ID, pid); err != nil {
		writeIdentityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *IdentityHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	if _, err := h.store.Authorize(r.Context(), p, pid, identity.PermMemberRead); err != nil {
		writeIdentityError(w, err)
		return
	}
	members, err := h.store.ListMembers(r.Context(), pid)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, members)
}

func (h *IdentityHandler) SetMember(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	actorRole, err := h.store.Authorize(r.Context(), p, pid, identity.PermMemberManage)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	targetUserID := r.PathValue("userId")
	var in struct {
		UserID   string `json:"user_id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := readJSON(w, r, 4*1024, &in); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid member payload")
		return
	}
	if targetUserID == "" {
		targetUserID = in.UserID
	}
	if targetUserID == "" && in.Username != "" {
		u, uErr := h.store.GetUserByUsername(r.Context(), in.Username)
		if uErr != nil {
			writeIdentityError(w, uErr)
			return
		}
		targetUserID = u.ID
	}
	if targetUserID == "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "user_id or username is required")
		return
	}
	m, err := h.store.SetMemberRole(r.Context(), p.User.ID, actorRole, pid, targetUserID, in.Role)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, m)
}

func (h *IdentityHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	actorRole, err := h.store.Authorize(r.Context(), p, pid, identity.PermMemberManage)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	if err := h.store.RemoveMember(r.Context(), p.User.ID, actorRole, pid, r.PathValue("userId")); err != nil {
		writeIdentityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *IdentityHandler) ListInvitations(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	if _, err := h.store.Authorize(r.Context(), p, pid, identity.PermInviteManage); err != nil {
		writeIdentityError(w, err)
		return
	}
	invs, err := h.store.ListInvitations(r.Context(), pid)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, invs)
}

func (h *IdentityHandler) CreateInvitation(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	actorRole, err := h.store.Authorize(r.Context(), p, pid, identity.PermInviteManage)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	var in struct {
		Email      string `json:"email"`
		Username   string `json:"username"`
		Role       string `json:"role"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := readJSON(w, r, 4*1024, &in); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid invitation payload")
		return
	}
	if in.Role == identity.RoleOwner && actorRole != identity.RoleOwner {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "only owners can invite owners")
		return
	}
	ttl := time.Duration(in.TTLSeconds) * time.Second
	inv, err := h.store.CreateInvitation(r.Context(), p.User.ID, pid, in.Email, in.Username, in.Role, ttl)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, inv)
}

func (h *IdentityHandler) RevokeInvitation(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	if _, err := h.store.Authorize(r.Context(), p, pid, identity.PermInviteManage); err != nil {
		writeIdentityError(w, err)
		return
	}
	if err := h.store.RevokeInvitation(r.Context(), p.User.ID, pid, r.PathValue("inviteId")); err != nil {
		writeIdentityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *IdentityHandler) AcceptInvitation(w http.ResponseWriter, r *http.Request) {
	var in identity.AcceptInvitationInput
	if err := readJSON(w, r, 8*1024, &in); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid accept payload")
		return
	}
	var caller *identity.Principal
	if p, ok := auth.FromContext(r.Context()); ok {
		caller = &p
	}
	pm, createdUser, err := h.store.AcceptInvitation(r.Context(), caller, in)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	resp := map[string]any{"membership": pm}
	if createdUser != nil {
		resp["user"] = createdUser
		if sess, tok, sErr := h.store.IssueSession(r.Context(), createdUser.ID, identity.SessionCLI, "invite-accept", "", pm.ID, 24*time.Hour); sErr == nil {
			resp["session_token"] = tok
			resp["session"] = sess
		}
	}
	apierr.WriteJSON(w, http.StatusOK, resp)
}

func (h *IdentityHandler) ListAuditEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	if pid == "" {
		pid = strings.TrimSpace(r.URL.Query().Get("project_id"))
	}
	if pid != "" {
		if _, err := h.store.Authorize(r.Context(), p, pid, identity.PermAuditRead); err != nil {
			writeIdentityError(w, err)
			return
		}
	} else if !p.User.IsSystemAdmin {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "specify project_id or authenticate as system administrator")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	events, err := h.store.ListAuditEvents(r.Context(), pid, limit)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, events)
}

func (h *IdentityHandler) GitHubStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	st, err := h.store.GetGitHubStatus(r.Context(), p.User.ID)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, st)
}

func (h *IdentityHandler) SaveGitHubConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := requireSystemAdmin(w, r)
	if !ok {
		return
	}
	var cfg identity.GitHubConfig
	if err := readJSON(w, r, 8*1024, &cfg); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid github config")
		return
	}
	if err := h.store.SaveGitHubConfig(r.Context(), p.User.ID, cfg); err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]bool{"configured": true})
}

func (h *IdentityHandler) GitHubOAuthStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode        string `json:"mode"`
		RedirectURI string `json:"redirect_uri"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(w, r, 4*1024, &in); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid oauth start payload")
			return
		}
	}
	if in.Mode == "" {
		in.Mode = "connect"
	}
	userID := ""
	if p, ok := auth.FromContext(r.Context()); ok {
		userID = p.User.ID
	}
	start, err := h.store.StartGitHubOAuth(r.Context(), userID, in.Mode, in.RedirectURI)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, start)
}

func (h *IdentityHandler) GitHubOAuthExchange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code        string `json:"code"`
		State       string `json:"state"`
		RedirectURI string `json:"redirect_uri"`
		Kind        string `json:"kind"`
		Label       string `json:"label"`
	}
	if err := readJSON(w, r, 8*1024, &in); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid oauth exchange payload")
		return
	}
	callerID := ""
	if p, ok := auth.FromContext(r.Context()); ok {
		callerID = p.User.ID
	}
	res, err := h.store.CompleteGitHubOAuth(r.Context(), h.httpClient, callerID, in.Code, in.State, in.RedirectURI)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	out := map[string]any{
		"mode":   res.Mode,
		"user":   res.User,
		"github": res.GitHub,
	}
	if res.Mode == "login" {
		kind := in.Kind
		if kind == "" {
			kind = identity.SessionBrowser
		}
		sess, tok, err := h.store.IssueSession(r.Context(), res.User.ID, kind, in.Label, "", "", 8*time.Hour)
		if err != nil {
			writeIdentityError(w, err)
			return
		}
		out["session_token"] = tok
		out["session"] = sess
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

func (h *IdentityHandler) GitHubDisconnect(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.store.DisconnectGitHub(r.Context(), p.User.ID); err != nil {
		writeIdentityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *IdentityHandler) GitHubRepos(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	repos, err := h.store.ListUserRepos(r.Context(), h.httpClient, p.User.ID)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, repos)
}
