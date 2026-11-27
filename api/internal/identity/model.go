package identity

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

const (
	LocalAdminID   = "local-admin"
	DefaultProject = "default"

	RoleOwner     = "owner"
	RoleAdmin     = "admin"
	RoleDeveloper = "developer"
	RoleViewer    = "viewer"

	CloudModePersonal      = "personal"
	CloudModeCollaborative = "collaborative"

	RegistrationOwnerOnly  = "owner_only"
	RegistrationInviteOnly = "invite_only"
	RegistrationOpen       = "open"
	RegistrationDisabled   = "disabled"

	SessionBrowser      = "browser"
	SessionCLI          = "cli"
	SessionPairedDevice = "paired_device"
)

type Permission string

const (
	PermProjectRead    Permission = "project:read"
	PermProjectUpdate  Permission = "project:update"
	PermProjectDelete  Permission = "project:delete"
	PermMemberRead     Permission = "member:read"
	PermMemberManage   Permission = "member:manage"
	PermInviteManage   Permission = "invite:manage"
	PermWorkspaceRead  Permission = "workspace:read"
	PermWorkspaceWrite Permission = "workspace:write"
	PermWorkloadRead   Permission = "workload:read"
	PermWorkloadWrite  Permission = "workload:write"
	PermAuditRead      Permission = "audit:read"
)

var rolePermissions = map[string]map[Permission]bool{
	RoleOwner: {
		PermProjectRead: true, PermProjectUpdate: true, PermProjectDelete: true,
		PermMemberRead: true, PermMemberManage: true, PermInviteManage: true,
		PermWorkspaceRead: true, PermWorkspaceWrite: true,
		PermWorkloadRead: true, PermWorkloadWrite: true,
		PermAuditRead: true,
	},
	RoleAdmin: {
		PermProjectRead: true, PermProjectUpdate: true,
		PermMemberRead: true, PermMemberManage: true, PermInviteManage: true,
		PermWorkspaceRead: true, PermWorkspaceWrite: true,
		PermWorkloadRead: true, PermWorkloadWrite: true,
		PermAuditRead: true,
	},
	RoleDeveloper: {
		PermProjectRead: true, PermMemberRead: true,
		PermWorkspaceRead: true, PermWorkspaceWrite: true,
		PermWorkloadRead: true, PermWorkloadWrite: true,
	},
	RoleViewer: {
		PermProjectRead: true, PermMemberRead: true,
		PermWorkspaceRead: true, PermWorkloadRead: true,
	},
}

var orderedPermissions = []Permission{
	PermProjectRead, PermProjectUpdate, PermProjectDelete,
	PermMemberRead, PermMemberManage, PermInviteManage,
	PermWorkspaceRead, PermWorkspaceWrite,
	PermWorkloadRead, PermWorkloadWrite, PermAuditRead,
}

func ValidRole(role string) bool {
	_, ok := rolePermissions[role]
	return ok
}

func ValidInviteRole(role string) bool {
	return role == RoleAdmin || role == RoleDeveloper || role == RoleViewer
}

func HasPermission(role string, perm Permission) bool {
	perms, ok := rolePermissions[role]
	return ok && perms[perm]
}

func PermittedOperations(role string) []string {
	perms := rolePermissions[role]
	out := make([]string, 0, len(orderedPermissions))
	for _, p := range orderedPermissions {
		if perms[p] {
			out = append(out, string(p))
		}
	}
	return out
}

var (
	usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}[a-z0-9]$`)
	projectSlugRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	emailPattern    = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("already exists or conflict")
	ErrForbidden       = errors.New("forbidden")
	ErrUnauthorized    = errors.New("unauthorized")
	ErrBadInput        = errors.New("invalid input")
	ErrPersonalMode    = errors.New("personal cloud mode restricts collaboration")
	ErrInviteExpired   = errors.New("invitation expired")
	ErrInviteUsed      = errors.New("invitation already accepted")
	ErrInviteRevoked   = errors.New("invitation revoked")
	ErrRateLimited     = errors.New("too many attempts")
	ErrGitHubNotReady  = errors.New("github oauth is not configured")
	ErrGitHubNotLinked = errors.New("github account is not connected")
	ErrRegistrationOff = errors.New("local account registration is restricted to the owner")
)

func NormalizeUsername(raw string) (string, error) {
	u := strings.ToLower(strings.TrimSpace(raw))
	if !usernamePattern.MatchString(u) {
		return "", errors.New("username must be 3-64 lowercase letters, digits, ., _, or -")
	}
	return u, nil
}

func NormalizeEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if e == "" {
		return "", nil
	}
	if len(e) > 254 || !emailPattern.MatchString(e) {
		return "", errors.New("invalid email address")
	}
	return e, nil
}

func SlugifyProject(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" || !projectSlugRe.MatchString(slug) {
		return "project"
	}
	return slug
}

type User struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	Email         string    `json:"email,omitempty"`
	DisplayName   string    `json:"display_name"`
	IsSystemAdmin bool      `json:"is_system_admin"`
	Disabled      bool      `json:"disabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description,omitempty"`
	OwnerID     string    `json:"owner_id"`
	IsPersonal  bool      `json:"is_personal"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ProjectMembership struct {
	Project
	Role                string   `json:"role"`
	PermittedOperations []string `json:"permitted_operations"`
}

type ProjectMember struct {
	ProjectID   string    `json:"project_id"`
	UserID      string    `json:"user_id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Email       string    `json:"email,omitempty"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Invitation struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Email      string     `json:"email,omitempty"`
	Username   string     `json:"username,omitempty"`
	Role       string     `json:"role"`
	InvitedBy  string     `json:"invited_by"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	AcceptedBy string     `json:"accepted_by,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	Token      string     `json:"token,omitempty"`
}

type Session struct {
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	Kind            string    `json:"kind"`
	DeviceID        string    `json:"device_id,omitempty"`
	Label           string    `json:"label,omitempty"`
	ActiveProjectID string    `json:"active_project_id,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	CreatedAt       time.Time `json:"created_at"`
}

type Principal struct {
	User            User
	SessionID       string
	AuthMethod      string // "operator_token", "session", "paired_device"
	ActiveProjectID string
	DeviceID        string
	CSRFToken       string
}

type Settings struct {
	CloudMode          string `json:"cloud_mode"`
	RegistrationPolicy string `json:"registration_policy"`
}

type GitHubStatus struct {
	Configured bool       `json:"configured"`
	Connected  bool       `json:"connected"`
	Login      string     `json:"login,omitempty"`
	Scopes     string     `json:"scopes,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

type GitHubRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description,omitempty"`
	Permissions   struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
		Pull  bool `json:"pull"`
	} `json:"permissions"`
}

type AuditEvent struct {
	ID          int64     `json:"id"`
	At          time.Time `json:"at"`
	ActorUserID string    `json:"actor_user_id,omitempty"`
	ProjectID   string    `json:"project_id,omitempty"`
	Action      string    `json:"action"`
	TargetType  string    `json:"target_type,omitempty"`
	TargetID    string    `json:"target_id,omitempty"`
	Outcome     string    `json:"outcome"`
	Detail      string    `json:"detail,omitempty"`
}
