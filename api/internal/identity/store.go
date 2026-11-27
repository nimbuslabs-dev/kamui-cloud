package identity

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Store struct {
	db            *sql.DB
	encKey        [32]byte
	adminPassword string
	Now           func() time.Time

	rateMu        sync.Mutex
	loginFailures map[string]rateWindow
}

type rateWindow struct {
	count   int
	resetAt time.Time
}

func NewStore(db *sql.DB, masterSecret, adminPassword string) *Store {
	return &Store{
		db:            db,
		encKey:        DeriveEncryptionKey(masterSecret),
		adminPassword: adminPassword,
		Now:           func() time.Time { return time.Now().UTC() },
		loginFailures: make(map[string]rateWindow),
	}
}

func (s *Store) now() time.Time {
	return s.Now().UTC()
}

func (s *Store) EnsureAdminBootstrap(ctx context.Context) error {
	if s.adminPassword == "" {
		return nil
	}
	var existingHash string
	err := s.db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id=?", LocalAdminID).Scan(&existingHash)
	if err != nil {
		return err
	}
	if existingHash != "" {
		return nil
	}
	hashed, err := HashPassword(s.adminPassword)
	if err != nil {
		return err
	}
	now := s.now().UnixNano()
	_, err = s.db.ExecContext(ctx, "UPDATE users SET password_hash=?, updated_at=? WHERE id=? AND password_hash=''", hashed, now, LocalAdminID)
	return err
}

func (s *Store) checkRateLimit(key string) error {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	w := s.loginFailures[key]
	if now.After(w.resetAt) {
		return nil
	}
	if w.count >= 10 {
		return ErrRateLimited
	}
	return nil
}

func (s *Store) recordFailure(key string) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := s.now()
	w := s.loginFailures[key]
	if now.After(w.resetAt) {
		w = rateWindow{resetAt: now.Add(time.Minute)}
	}
	w.count++
	s.loginFailures[key] = w
}

func (s *Store) clearFailures(key string) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	delete(s.loginFailures, key)
}

func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	out := Settings{CloudMode: CloudModePersonal, RegistrationPolicy: RegistrationOwnerOnly}
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM system_settings WHERE key IN ('cloud_mode', 'registration_policy')")
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return out, err
		}
		switch k {
		case "cloud_mode":
			if v == CloudModePersonal || v == CloudModeCollaborative {
				out.CloudMode = v
			}
		case "registration_policy":
			out.RegistrationPolicy = v
		}
	}
	return out, rows.Err()
}

func (s *Store) UpdateSettings(ctx context.Context, actorID string, next Settings) (Settings, error) {
	current, err := s.GetSettings(ctx)
	if err != nil {
		return Settings{}, err
	}
	if next.CloudMode != "" {
		if next.CloudMode != CloudModePersonal && next.CloudMode != CloudModeCollaborative {
			return Settings{}, fmt.Errorf("%w: cloud_mode must be personal or collaborative", ErrBadInput)
		}
		current.CloudMode = next.CloudMode
	}
	if next.RegistrationPolicy != "" {
		switch next.RegistrationPolicy {
		case RegistrationOwnerOnly, RegistrationInviteOnly, RegistrationOpen, RegistrationDisabled:
			current.RegistrationPolicy = next.RegistrationPolicy
		default:
			return Settings{}, fmt.Errorf("%w: invalid registration_policy", ErrBadInput)
		}
	}
	now := s.now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Settings{}, err
	}
	defer tx.Rollback()
	for _, kv := range [][2]string{
		{"cloud_mode", current.CloudMode},
		{"registration_policy", current.RegistrationPolicy},
	} {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO system_settings(key, value, updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at",
			kv[0], kv[1], now,
		); err != nil {
			return Settings{}, err
		}
	}
	if err := insertAuditTx(ctx, tx, s.now(), actorID, "", "identity.settings_update", "system", "settings", "success", current.CloudMode+","+current.RegistrationPolicy); err != nil {
		return Settings{}, err
	}
	return current, tx.Commit()
}

type CreateUserInput struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

func (s *Store) CreateUser(ctx context.Context, actorID string, in CreateUserInput, createPersonalProject bool) (User, *Project, error) {
	username, err := NormalizeUsername(in.Username)
	if err != nil {
		return User{}, nil, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return User{}, nil, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	hash, err := HashPassword(in.Password)
	if err != nil {
		return User{}, nil, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = username
	}
	if len(display) > 100 || strings.ContainsRune(display, 0) {
		return User{}, nil, fmt.Errorf("%w: display_name must be 1-100 bytes", ErrBadInput)
	}
	id, err := NewSecretToken("usr-", 12)
	if err != nil {
		return User{}, nil, err
	}
	now := s.now()
	ts := now.UnixNano()
	u := User{
		ID: id, Username: username, Email: email, DisplayName: display,
		IsSystemAdmin: false, Disabled: false, CreatedAt: now, UpdatedAt: now,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users(id, username, email, display_name, password_hash, is_system_admin, disabled, created_at, updated_at)
		 VALUES(?,?,?,?,?,0,0,?,?)`,
		u.ID, u.Username, u.Email, u.DisplayName, hash, ts, ts,
	); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return User{}, nil, ErrConflict
		}
		return User{}, nil, err
	}
	var personal *Project
	if createPersonalProject {
		proj, err := createProjectTx(ctx, tx, now, u.ID, u.Username+" Personal", "Personal workspace project", true)
		if err != nil {
			return User{}, nil, err
		}
		personal = &proj
	}
	if err := insertAuditTx(ctx, tx, now, actorID, "", "auth.register", "user", u.ID, "success", u.Username); err != nil {
		return User{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, nil, err
	}
	return u, personal, nil
}

func scanUser(row interface{ Scan(...any) error }) (User, string, error) {
	var u User
	var hash string
	var adminInt, disabledInt int
	var createdAt, updatedAt int64
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &hash, &adminInt, &disabledInt, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", err
	}
	u.IsSystemAdmin = adminInt != 0
	u.Disabled = disabledInt != 0
	u.CreatedAt = time.Unix(0, createdAt).UTC()
	u.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return u, hash, nil
}

const userSelect = `SELECT id, COALESCE(username,''), COALESCE(email,''), COALESCE(display_name,''), COALESCE(password_hash,''), is_system_admin, disabled, created_at, updated_at FROM users`

func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	u, _, err := scanUser(s.db.QueryRowContext(ctx, userSelect+" WHERE id=?", id))
	return u, err
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	norm := strings.ToLower(strings.TrimSpace(username))
	u, _, err := scanUser(s.db.QueryRowContext(ctx, userSelect+" WHERE lower(username)=?", norm))
	return u, err
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, userSelect+" ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]User, 0)
	for rows.Next() {
		u, _, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) SetUserDisabled(ctx context.Context, actorID, userID string, disabled bool) (User, error) {
	if userID == LocalAdminID && disabled {
		return User{}, fmt.Errorf("%w: cannot disable the local administrator", ErrBadInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	now := s.now()
	disInt := 0
	if disabled {
		disInt = 1
	}
	res, err := tx.ExecContext(ctx, "UPDATE users SET disabled=?, updated_at=? WHERE id=?", disInt, now.UnixNano(), userID)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
	}
	if disabled {
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", now.UnixNano(), userID); err != nil {
			return User{}, err
		}
	}
	action := "user.enable"
	if disabled {
		action = "user.disable"
	}
	if err := insertAuditTx(ctx, tx, now, actorID, "", action, "user", userID, "success", ""); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, userID)
}

func (s *Store) AuthenticateCredentials(ctx context.Context, username, password, rateKey string) (User, error) {
	if rateKey != "" {
		if err := s.checkRateLimit(rateKey); err != nil {
			return User{}, err
		}
	}
	norm := strings.ToLower(strings.TrimSpace(username))
	if norm == "" {
		norm = "admin"
	}
	u, hash, err := scanUser(s.db.QueryRowContext(ctx, userSelect+" WHERE lower(username)=?", norm))
	ok := false
	if err == nil && !u.Disabled {
		ok = VerifyPassword(password, hash)
		if !ok && u.ID == LocalAdminID && s.adminPassword != "" {
			ok = subtle.ConstantTimeCompare([]byte(password), []byte(s.adminPassword)) == 1
		}
	}
	if !ok {
		if rateKey != "" {
			s.recordFailure(rateKey)
		}
		_ = s.RecordAudit(ctx, "", "", "auth.login_failed", "user", norm, "failure", "invalid credentials")
		return User{}, ErrUnauthorized
	}
	if rateKey != "" {
		s.clearFailures(rateKey)
	}
	return u, nil
}

func (s *Store) IssueSession(ctx context.Context, userID, kind, label, deviceID, activeProjectID string, ttl time.Duration) (Session, string, error) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if kind != SessionBrowser && kind != SessionCLI && kind != SessionPairedDevice {
		return Session{}, "", fmt.Errorf("%w: invalid session kind", ErrBadInput)
	}
	token, err := NewSecretToken("ksess_", 32)
	if err != nil {
		return Session{}, "", err
	}
	csrf, err := NewSecretToken("", 32)
	if err != nil {
		return Session{}, "", err
	}
	id, err := NewSecretToken("ses-", 12)
	if err != nil {
		return Session{}, "", err
	}
	if activeProjectID == "" {
		projects, err := s.ListProjectsForUser(ctx, userID)
		if err == nil && len(projects) > 0 {
			activeProjectID = projects[0].ID
		}
	}
	now := s.now()
	expires := now.Add(ttl)
	var projArg any
	if activeProjectID != "" {
		projArg = activeProjectID
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, token_hash, csrf_token, kind, device_id, label, active_project_id, expires_at, last_seen_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, userID, HashToken(token), csrf, kind, deviceID, strings.TrimSpace(label), projArg,
		expires.UnixNano(), now.UnixNano(), now.UnixNano(),
	)
	if err != nil {
		return Session{}, "", err
	}
	_ = s.RecordAudit(ctx, userID, activeProjectID, "auth.login", "session", id, "success", kind)
	return Session{
		ID: id, UserID: userID, Kind: kind, DeviceID: deviceID, Label: strings.TrimSpace(label),
		ActiveProjectID: activeProjectID, ExpiresAt: expires, LastSeenAt: now, CreatedAt: now,
	}, token, nil
}

func (s *Store) AuthenticateToken(ctx context.Context, rawToken string) (Principal, error) {
	if rawToken == "" {
		return Principal{}, ErrUnauthorized
	}
	hash := HashToken(rawToken)
	now := s.now()
	var sessID, userID, csrf, kind, deviceID, activeProj string
	var expiresAt int64
	var revokedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, csrf_token, kind, device_id, COALESCE(active_project_id,''), expires_at, revoked_at
		 FROM sessions WHERE token_hash=?`,
		hash,
	).Scan(&sessID, &userID, &csrf, &kind, &deviceID, &activeProj, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	if revokedAt.Valid || now.UnixNano() >= expiresAt {
		return Principal{}, ErrUnauthorized
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil || u.Disabled {
		return Principal{}, ErrUnauthorized
	}
	if deviceID != "" {
		var devStatus string
		err := s.db.QueryRowContext(ctx, "SELECT status FROM paired_devices WHERE id=?", deviceID).Scan(&devStatus)
		if err != nil || devStatus != "approved" {
			return Principal{}, ErrUnauthorized
		}
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE sessions SET last_seen_at=? WHERE id=?", now.UnixNano(), sessID)
	method := "session"
	if kind == SessionPairedDevice {
		method = SessionPairedDevice
	}
	return Principal{
		User:            u,
		SessionID:       sessID,
		AuthMethod:      method,
		ActiveProjectID: activeProj,
		DeviceID:        deviceID,
		CSRFToken:       csrf,
	}, nil
}

func (s *Store) RevokeSessionByID(ctx context.Context, actorID, sessionID string) error {
	now := s.now()
	res, err := s.db.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE id=? AND revoked_at IS NULL", now.UnixNano(), sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.RecordAudit(ctx, actorID, "", "auth.logout", "session", sessionID, "success", "")
}

func (s *Store) RevokeAllUserSessions(ctx context.Context, userID string) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", now.UnixNano(), userID)
	if err != nil {
		return err
	}
	return s.RecordAudit(ctx, userID, "", "auth.logout_all", "user", userID, "success", "")
}

func (s *Store) ListUserSessions(ctx context.Context, userID string) ([]Session, error) {
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, kind, device_id, label, COALESCE(active_project_id,''), expires_at, last_seen_at, created_at
		 FROM sessions WHERE user_id=? AND revoked_at IS NULL AND expires_at>? ORDER BY created_at DESC`,
		userID, now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Session, 0)
	for rows.Next() {
		var sess Session
		var exp, seen, created int64
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Kind, &sess.DeviceID, &sess.Label, &sess.ActiveProjectID, &exp, &seen, &created); err != nil {
			return nil, err
		}
		sess.ExpiresAt = time.Unix(0, exp).UTC()
		sess.LastSeenAt = time.Unix(0, seen).UTC()
		sess.CreatedAt = time.Unix(0, created).UTC()
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) SetActiveProject(ctx context.Context, sessionID, userID, projectID string) error {
	role, err := s.GetMemberRole(ctx, projectID, userID)
	if err != nil || role == "" {
		return ErrForbidden
	}
	_, err = s.db.ExecContext(ctx, "UPDATE sessions SET active_project_id=? WHERE id=? AND user_id=?", projectID, sessionID, userID)
	return err
}

func createProjectTx(ctx context.Context, tx *sql.Tx, now time.Time, ownerID, name, description string, isPersonal bool) (Project, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len(trimmed) > 100 || strings.ContainsRune(trimmed, 0) {
		return Project{}, fmt.Errorf("%w: project name must be 1-100 bytes", ErrBadInput)
	}
	id, err := NewSecretToken("prj-", 12)
	if err != nil {
		return Project{}, err
	}
	slug := SlugifyProject(trimmed)
	ts := now.UnixNano()
	personalInt := 0
	if isPersonal {
		personalInt = 1
	}
	p := Project{
		ID: id, Name: trimmed, Slug: slug, Description: strings.TrimSpace(description),
		OwnerID: ownerID, IsPersonal: isPersonal, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO projects(id, owner_id, name, slug, description, is_personal, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, p.OwnerID, p.Name, p.Slug, p.Description, personalInt, ts, ts,
	); err != nil {
		return Project{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO project_members(project_id, user_id, role, created_at, updated_at) VALUES(?,?,?,?,?)`,
		p.ID, ownerID, RoleOwner, ts, ts,
	); err != nil {
		return Project{}, err
	}
	return p, nil
}

func (s *Store) CreateProject(ctx context.Context, ownerID, name, description string, isPersonal bool) (ProjectMembership, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProjectMembership{}, err
	}
	defer tx.Rollback()
	p, err := createProjectTx(ctx, tx, now, ownerID, name, description, isPersonal)
	if err != nil {
		return ProjectMembership{}, err
	}
	if err := insertAuditTx(ctx, tx, now, ownerID, p.ID, "project.create", "project", p.ID, "success", p.Name); err != nil {
		return ProjectMembership{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProjectMembership{}, err
	}
	return ProjectMembership{Project: p, Role: RoleOwner, PermittedOperations: PermittedOperations(RoleOwner)}, nil
}

func (s *Store) GetProjectForUser(ctx context.Context, projectID, userID string) (ProjectMembership, error) {
	var pm ProjectMembership
	var personalInt int
	var created, updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT p.id, p.name, p.slug, p.description, p.owner_id, p.is_personal, p.created_at, p.updated_at, m.role
		 FROM projects p
		 JOIN project_members m ON m.project_id = p.id
		 WHERE p.id=? AND m.user_id=?`,
		projectID, userID,
	).Scan(&pm.ID, &pm.Name, &pm.Slug, &pm.Description, &pm.OwnerID, &personalInt, &created, &updated, &pm.Role)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if s.db.QueryRowContext(ctx, "SELECT 1 FROM projects WHERE id=?", projectID).Scan(&exists) == nil {
			return ProjectMembership{}, ErrForbidden
		}
		return ProjectMembership{}, ErrNotFound
	}
	if err != nil {
		return ProjectMembership{}, err
	}
	pm.IsPersonal = personalInt != 0
	pm.CreatedAt = time.Unix(0, created).UTC()
	pm.UpdatedAt = time.Unix(0, updated).UTC()
	pm.PermittedOperations = PermittedOperations(pm.Role)
	return pm, nil
}

func (s *Store) ListProjectsForUser(ctx context.Context, userID string) ([]ProjectMembership, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.name, p.slug, p.description, p.owner_id, p.is_personal, p.created_at, p.updated_at, m.role
		 FROM projects p
		 JOIN project_members m ON m.project_id = p.id
		 WHERE m.user_id=?
		 ORDER BY (p.id = 'default') DESC, p.created_at ASC, p.id ASC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ProjectMembership, 0)
	for rows.Next() {
		var pm ProjectMembership
		var personalInt int
		var created, updated int64
		if err := rows.Scan(&pm.ID, &pm.Name, &pm.Slug, &pm.Description, &pm.OwnerID, &personalInt, &created, &updated, &pm.Role); err != nil {
			return nil, err
		}
		pm.IsPersonal = personalInt != 0
		pm.CreatedAt = time.Unix(0, created).UTC()
		pm.UpdatedAt = time.Unix(0, updated).UTC()
		pm.PermittedOperations = PermittedOperations(pm.Role)
		out = append(out, pm)
	}
	return out, rows.Err()
}

func (s *Store) UserProjectIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT project_id FROM project_members WHERE user_id=?", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) GetMemberRole(ctx context.Context, projectID, userID string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, "SELECT role FROM project_members WHERE project_id=? AND user_id=?", projectID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func (s *Store) Authorize(ctx context.Context, p Principal, projectID string, perm Permission) (string, error) {
	if projectID == "" {
		projectID = DefaultProject
	}
	role, err := s.GetMemberRole(ctx, projectID, p.User.ID)
	if err != nil {
		return "", err
	}
	if role == "" || !HasPermission(role, perm) {
		_ = s.RecordAudit(ctx, p.User.ID, projectID, "authz.denied", "permission", string(perm), "denied", "role="+role)
		return role, ErrForbidden
	}
	return role, nil
}

func (s *Store) DeleteProject(ctx context.Context, actorID, projectID string) error {
	if projectID == DefaultProject {
		return fmt.Errorf("%w: cannot delete the default project", ErrBadInput)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM workloads WHERE project_id=?", projectID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: project still has workloads", ErrConflict)
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM projects WHERE id=?", projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.RecordAudit(ctx, actorID, projectID, "project.delete", "project", projectID, "success", "")
}

func (s *Store) ListMembers(ctx context.Context, projectID string) ([]ProjectMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.project_id, m.user_id, COALESCE(u.username,''), COALESCE(u.display_name,''), COALESCE(u.email,''), m.role, m.created_at, m.updated_at
		 FROM project_members m
		 JOIN users u ON u.id = m.user_id
		 WHERE m.project_id=?
		 ORDER BY m.created_at ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ProjectMember, 0)
	for rows.Next() {
		var m ProjectMember
		var created, updated int64
		if err := rows.Scan(&m.ProjectID, &m.UserID, &m.Username, &m.DisplayName, &m.Email, &m.Role, &created, &updated); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(0, created).UTC()
		m.UpdatedAt = time.Unix(0, updated).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SetMemberRole(ctx context.Context, actorID, actorRole, projectID, targetUserID, role string) (ProjectMember, error) {
	if !ValidRole(role) {
		return ProjectMember{}, fmt.Errorf("%w: invalid role", ErrBadInput)
	}
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return ProjectMember{}, err
	}
	if settings.CloudMode != CloudModeCollaborative && targetUserID != actorID {
		return ProjectMember{}, ErrPersonalMode
	}
	if role == RoleOwner && actorRole != RoleOwner {
		return ProjectMember{}, ErrForbidden
	}
	existingRole, err := s.GetMemberRole(ctx, projectID, targetUserID)
	if err != nil {
		return ProjectMember{}, err
	}
	if existingRole == RoleOwner && actorRole != RoleOwner {
		return ProjectMember{}, ErrForbidden
	}
	if existingRole == RoleOwner && role != RoleOwner {
		var owners int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM project_members WHERE project_id=? AND role='owner'", projectID).Scan(&owners); err != nil {
			return ProjectMember{}, err
		}
		if owners <= 1 {
			return ProjectMember{}, fmt.Errorf("%w: project must retain at least one owner", ErrConflict)
		}
	}
	if _, err := s.GetUser(ctx, targetUserID); err != nil {
		return ProjectMember{}, err
	}
	now := s.now()
	ts := now.UnixNano()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO project_members(project_id, user_id, role, created_at, updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(project_id, user_id) DO UPDATE SET role=excluded.role, updated_at=excluded.updated_at`,
		projectID, targetUserID, role, ts, ts,
	); err != nil {
		return ProjectMember{}, err
	}
	_ = s.RecordAudit(ctx, actorID, projectID, "member.role_set", "user", targetUserID, "success", role)
	members, err := s.ListMembers(ctx, projectID)
	if err != nil {
		return ProjectMember{}, err
	}
	for _, m := range members {
		if m.UserID == targetUserID {
			return m, nil
		}
	}
	return ProjectMember{}, ErrNotFound
}

func (s *Store) RemoveMember(ctx context.Context, actorID, actorRole, projectID, targetUserID string) error {
	targetRole, err := s.GetMemberRole(ctx, projectID, targetUserID)
	if err != nil {
		return err
	}
	if targetRole == "" {
		return ErrNotFound
	}
	if targetRole == RoleOwner && actorRole != RoleOwner {
		return ErrForbidden
	}
	if targetRole == RoleOwner {
		var owners int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM project_members WHERE project_id=? AND role='owner'", projectID).Scan(&owners); err != nil {
			return err
		}
		if owners <= 1 {
			return fmt.Errorf("%w: cannot remove the last project owner", ErrConflict)
		}
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM project_members WHERE project_id=? AND user_id=?", projectID, targetUserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.RecordAudit(ctx, actorID, projectID, "member.revoke", "user", targetUserID, "success", targetRole)
}

func (s *Store) CreateInvitation(ctx context.Context, actorID, projectID, email, username, role string, ttl time.Duration) (Invitation, error) {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return Invitation{}, err
	}
	if settings.CloudMode != CloudModeCollaborative {
		return Invitation{}, ErrPersonalMode
	}
	if !ValidInviteRole(role) {
		return Invitation{}, fmt.Errorf("%w: invitation role must be admin, developer, or viewer", ErrBadInput)
	}
	normEmail, err := NormalizeEmail(email)
	if err != nil {
		return Invitation{}, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	var normUser string
	if strings.TrimSpace(username) != "" {
		normUser, err = NormalizeUsername(username)
		if err != nil {
			return Invitation{}, fmt.Errorf("%w: %v", ErrBadInput, err)
		}
	}
	if normEmail == "" && normUser == "" {
		return Invitation{}, fmt.Errorf("%w: email or username is required", ErrBadInput)
	}
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	if ttl < 1*time.Second || ttl > 7*24*time.Hour {
		return Invitation{}, fmt.Errorf("%w: invitation TTL must be between 1 second and 7 days", ErrBadInput)
	}
	id, err := NewSecretToken("inv-", 12)
	if err != nil {
		return Invitation{}, err
	}
	rawToken, err := NewSecretToken("kinv_", 32)
	if err != nil {
		return Invitation{}, err
	}
	now := s.now()
	expires := now.Add(ttl)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO invitations(id, project_id, email, username, role, token_hash, invited_by, expires_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		id, projectID, normEmail, normUser, role, HashToken(rawToken), actorID, expires.UnixNano(), now.UnixNano(),
	)
	if err != nil {
		return Invitation{}, err
	}
	_ = s.RecordAudit(ctx, actorID, projectID, "member.invite", "invitation", id, "success", role)
	return Invitation{
		ID: id, ProjectID: projectID, Email: normEmail, Username: normUser, Role: role,
		InvitedBy: actorID, ExpiresAt: expires, CreatedAt: now, Token: rawToken,
	}, nil
}

func (s *Store) ListInvitations(ctx context.Context, projectID string) ([]Invitation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, email, username, role, invited_by, expires_at, accepted_at, COALESCE(accepted_by,''), revoked_at, created_at
		 FROM invitations WHERE project_id=? ORDER BY created_at DESC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Invitation, 0)
	for rows.Next() {
		var inv Invitation
		var exp, created int64
		var acc, rev sql.NullInt64
		if err := rows.Scan(&inv.ID, &inv.ProjectID, &inv.Email, &inv.Username, &inv.Role, &inv.InvitedBy, &exp, &acc, &inv.AcceptedBy, &rev, &created); err != nil {
			return nil, err
		}
		inv.ExpiresAt = time.Unix(0, exp).UTC()
		inv.CreatedAt = time.Unix(0, created).UTC()
		if acc.Valid {
			t := time.Unix(0, acc.Int64).UTC()
			inv.AcceptedAt = &t
		}
		if rev.Valid {
			t := time.Unix(0, rev.Int64).UTC()
			inv.RevokedAt = &t
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *Store) RevokeInvitation(ctx context.Context, actorID, projectID, inviteID string) error {
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx,
		"UPDATE invitations SET revoked_at=? WHERE id=? AND project_id=? AND accepted_at IS NULL AND revoked_at IS NULL",
		now, inviteID, projectID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.RecordAudit(ctx, actorID, projectID, "member.invite_revoke", "invitation", inviteID, "success", "")
}

type AcceptInvitationInput struct {
	Token       string `json:"token"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

func (s *Store) AcceptInvitation(ctx context.Context, caller *Principal, in AcceptInvitationInput) (ProjectMembership, *User, error) {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return ProjectMembership{}, nil, err
	}
	if settings.CloudMode != CloudModeCollaborative {
		return ProjectMembership{}, nil, ErrPersonalMode
	}
	if strings.TrimSpace(in.Token) == "" {
		return ProjectMembership{}, nil, fmt.Errorf("%w: invitation token is required", ErrBadInput)
	}
	hash := HashToken(strings.TrimSpace(in.Token))
	now := s.now()
	ts := now.UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProjectMembership{}, nil, err
	}
	defer tx.Rollback()

	var invID, projectID, invEmail, invUsername, role string
	var expiresAt int64
	var acceptedAt, revokedAt sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT id, project_id, email, username, role, expires_at, accepted_at, revoked_at
		 FROM invitations WHERE token_hash=?`,
		hash,
	).Scan(&invID, &projectID, &invEmail, &invUsername, &role, &expiresAt, &acceptedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectMembership{}, nil, ErrNotFound
	}
	if err != nil {
		return ProjectMembership{}, nil, err
	}
	if revokedAt.Valid {
		return ProjectMembership{}, nil, ErrInviteRevoked
	}
	if acceptedAt.Valid {
		return ProjectMembership{}, nil, ErrInviteUsed
	}
	if ts >= expiresAt {
		return ProjectMembership{}, nil, ErrInviteExpired
	}

	var targetUserID string
	var createdUser *User
	if caller != nil {
		if invUsername != "" && caller.User.Username != invUsername {
			return ProjectMembership{}, nil, fmt.Errorf("%w: invitation is restricted to username %s", ErrForbidden, invUsername)
		}
		if invEmail != "" && caller.User.Email != "" && caller.User.Email != invEmail {
			return ProjectMembership{}, nil, fmt.Errorf("%w: invitation is restricted to email %s", ErrForbidden, invEmail)
		}
		targetUserID = caller.User.ID
	} else {
		if settings.RegistrationPolicy == RegistrationDisabled {
			return ProjectMembership{}, nil, ErrRegistrationOff
		}
		reqUser := in.Username
		if reqUser == "" {
			reqUser = invUsername
		}
		normUser, err := NormalizeUsername(reqUser)
		if err != nil {
			return ProjectMembership{}, nil, fmt.Errorf("%w: %v", ErrBadInput, err)
		}
		if invUsername != "" && normUser != invUsername {
			return ProjectMembership{}, nil, fmt.Errorf("%w: invitation is restricted to username %s", ErrForbidden, invUsername)
		}
		passHash, err := HashPassword(in.Password)
		if err != nil {
			return ProjectMembership{}, nil, fmt.Errorf("%w: %v", ErrBadInput, err)
		}
		display := strings.TrimSpace(in.DisplayName)
		if display == "" {
			display = normUser
		}
		uid, err := NewSecretToken("usr-", 12)
		if err != nil {
			return ProjectMembership{}, nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users(id, username, email, display_name, password_hash, is_system_admin, disabled, created_at, updated_at)
			 VALUES(?,?,?,?,?,0,0,?,?)`,
			uid, normUser, invEmail, display, passHash, ts, ts,
		); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ProjectMembership{}, nil, ErrConflict
			}
			return ProjectMembership{}, nil, err
		}
		targetUserID = uid
		createdUser = &User{
			ID: uid, Username: normUser, Email: invEmail, DisplayName: display,
			CreatedAt: now, UpdatedAt: now,
		}
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE invitations SET accepted_at=?, accepted_by=?
		 WHERE id=? AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at>?`,
		ts, targetUserID, invID, ts,
	)
	if err != nil {
		return ProjectMembership{}, nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ProjectMembership{}, nil, ErrInviteUsed
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO project_members(project_id, user_id, role, created_at, updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(project_id, user_id) DO UPDATE SET role=excluded.role, updated_at=excluded.updated_at`,
		projectID, targetUserID, role, ts, ts,
	); err != nil {
		return ProjectMembership{}, nil, err
	}
	if err := insertAuditTx(ctx, tx, now, targetUserID, projectID, "member.accept", "invitation", invID, "success", role); err != nil {
		return ProjectMembership{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return ProjectMembership{}, nil, err
	}
	pm, err := s.GetProjectForUser(ctx, projectID, targetUserID)
	return pm, createdUser, err
}

func insertAuditTx(ctx context.Context, tx *sql.Tx, now time.Time, actor, projectID, action, targetType, targetID, outcome, detail string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit_events(at, actor_user_id, project_id, action, target_type, target_id, outcome, detail)
		 VALUES(?,?,?,?,?,?,?,?)`,
		now.UnixNano(), actor, projectID, action, targetType, targetID, outcome, detail,
	)
	return err
}

func (s *Store) RecordAudit(ctx context.Context, actor, projectID, action, targetType, targetID, outcome, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_events(at, actor_user_id, project_id, action, target_type, target_id, outcome, detail)
		 VALUES(?,?,?,?,?,?,?,?)`,
		s.now().UnixNano(), actor, projectID, action, targetType, targetID, outcome, detail,
	)
	return err
}

func (s *Store) ListAuditEvents(ctx context.Context, projectID string, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := "SELECT id, at, actor_user_id, project_id, action, target_type, target_id, outcome, detail FROM audit_events"
	var args []any
	if projectID != "" {
		query += " WHERE project_id=?"
		args = append(args, projectID)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuditEvent, 0)
	for rows.Next() {
		var e AuditEvent
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.ActorUserID, &e.ProjectID, &e.Action, &e.TargetType, &e.TargetID, &e.Outcome, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(0, at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
