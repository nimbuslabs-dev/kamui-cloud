package identity

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

type GitHubConfig struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	CallbackURL  string `json:"callback_url"`
	OAuthBaseURL string `json:"oauth_base_url"`
	APIBaseURL   string `json:"api_base_url"`
}

func LoadGitHubEnvConfig() GitHubConfig {
	return GitHubConfig{
		ClientID:     strings.TrimSpace(os.Getenv("KAMUI_GITHUB_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("KAMUI_GITHUB_CLIENT_SECRET")),
		CallbackURL:  strings.TrimSpace(os.Getenv("KAMUI_GITHUB_CALLBACK_URL")),
		OAuthBaseURL: fallbackStr(strings.TrimSpace(os.Getenv("KAMUI_GITHUB_OAUTH_URL")), "https://github.com"),
		APIBaseURL:   fallbackStr(strings.TrimSpace(os.Getenv("KAMUI_GITHUB_API_URL")), "https://api.github.com"),
	}
}

func fallbackStr(v, def string) string {
	if v == "" {
		return def
	}
	return strings.TrimRight(v, "/")
}

func ValidateHTTPSOrLoopbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if u.Scheme == "http" && err == nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("HTTPS is required except for loopback testing")
}

func (s *Store) GetGitHubConfig(ctx context.Context) (GitHubConfig, error) {
	cfg := LoadGitHubEnvConfig()
	rows, err := s.db.QueryContext(ctx,
		"SELECT key, value FROM system_settings WHERE key IN ('github_client_id','github_client_secret_enc','github_callback_url','github_oauth_url','github_api_url')",
	)
	if err != nil {
		return cfg, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return cfg, err
		}
		if v == "" {
			continue
		}
		switch k {
		case "github_client_id":
			cfg.ClientID = v
		case "github_client_secret_enc":
			if plain, err := Open(s.encKey, v); err == nil {
				cfg.ClientSecret = plain
			}
		case "github_callback_url":
			cfg.CallbackURL = v
		case "github_oauth_url":
			cfg.OAuthBaseURL = strings.TrimRight(v, "/")
		case "github_api_url":
			cfg.APIBaseURL = strings.TrimRight(v, "/")
		}
	}
	return cfg, rows.Err()
}

func (s *Store) SaveGitHubConfig(ctx context.Context, actorID string, cfg GitHubConfig) error {
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" || strings.TrimSpace(cfg.CallbackURL) == "" {
		return fmt.Errorf("%w: client_id, client_secret, and callback_url are required", ErrBadInput)
	}
	if err := ValidateHTTPSOrLoopbackURL(cfg.CallbackURL); err != nil {
		return fmt.Errorf("%w: callback_url: %v", ErrBadInput, err)
	}
	oauthURL := fallbackStr(strings.TrimSpace(cfg.OAuthBaseURL), "https://github.com")
	apiURL := fallbackStr(strings.TrimSpace(cfg.APIBaseURL), "https://api.github.com")
	if err := ValidateHTTPSOrLoopbackURL(oauthURL); err != nil {
		return fmt.Errorf("%w: oauth_base_url: %v", ErrBadInput, err)
	}
	if err := ValidateHTTPSOrLoopbackURL(apiURL); err != nil {
		return fmt.Errorf("%w: api_base_url: %v", ErrBadInput, err)
	}
	encSecret, err := Seal(s.encKey, cfg.ClientSecret)
	if err != nil {
		return err
	}
	now := s.now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, kv := range [][2]string{
		{"github_client_id", strings.TrimSpace(cfg.ClientID)},
		{"github_client_secret_enc", encSecret},
		{"github_callback_url", strings.TrimSpace(cfg.CallbackURL)},
		{"github_oauth_url", oauthURL},
		{"github_api_url", apiURL},
	} {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO system_settings(key, value, updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at",
			kv[0], kv[1], now,
		); err != nil {
			return err
		}
	}
	if err := insertAuditTx(ctx, tx, s.now(), actorID, "", "github.config_update", "system", "github", "success", cfg.ClientID); err != nil {
		return err
	}
	return tx.Commit()
}

type OAuthStart struct {
	AuthorizeURL string    `json:"authorize_url"`
	State        string    `json:"state"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (s *Store) StartGitHubOAuth(ctx context.Context, userID, mode, redirectURI string) (OAuthStart, error) {
	cfg, err := s.GetGitHubConfig(ctx)
	if err != nil {
		return OAuthStart{}, err
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return OAuthStart{}, ErrGitHubNotReady
	}
	if mode != "login" && mode != "connect" {
		return OAuthStart{}, fmt.Errorf("%w: mode must be login or connect", ErrBadInput)
	}
	if mode == "connect" && userID == "" {
		return OAuthStart{}, ErrUnauthorized
	}
	cb := strings.TrimSpace(redirectURI)
	if cb == "" {
		cb = cfg.CallbackURL
	}
	if cb == "" {
		return OAuthStart{}, fmt.Errorf("%w: redirect_uri is required", ErrBadInput)
	}
	if err := ValidateHTTPSOrLoopbackURL(cb); err != nil {
		return OAuthStart{}, fmt.Errorf("%w: invalid redirect_uri: %v", ErrBadInput, err)
	}
	if cfg.CallbackURL != "" && subtle.ConstantTimeCompare([]byte(cb), []byte(cfg.CallbackURL)) != 1 {
		return OAuthStart{}, fmt.Errorf("%w: redirect_uri does not match configured callback URL", ErrBadInput)
	}

	state, err := NewSecretToken("ghs_", 24)
	if err != nil {
		return OAuthStart{}, err
	}
	verifier, err := NewSecretToken("", 32)
	if err != nil {
		return OAuthStart{}, err
	}
	now := s.now()
	expires := now.Add(10 * time.Minute)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO oauth_states(state_hash, user_id, code_verifier, redirect_uri, mode, expires_at, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		HashToken(state), userID, verifier, cb, mode, expires.UnixNano(), now.UnixNano(),
	)
	if err != nil {
		return OAuthStart{}, err
	}
	params := url.Values{
		"client_id":             {cfg.ClientID},
		"redirect_uri":          {cb},
		"scope":                 {"read:user repo"},
		"state":                 {state},
		"code_challenge":        {PKCEChallengeS256(verifier)},
		"code_challenge_method": {"S256"},
	}
	return OAuthStart{
		AuthorizeURL: cfg.OAuthBaseURL + "/login/oauth/authorize?" + params.Encode(),
		State:        state,
		ExpiresAt:    expires,
	}, nil
}

type OAuthResult struct {
	Mode   string       `json:"mode"`
	User   User         `json:"user"`
	GitHub GitHubStatus `json:"github"`
}

func (s *Store) CompleteGitHubOAuth(ctx context.Context, httpClient *http.Client, callerUserID, code, state, redirectURI string) (OAuthResult, error) {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(state) == "" {
		return OAuthResult{}, fmt.Errorf("%w: code and state are required", ErrBadInput)
	}
	cfg, err := s.GetGitHubConfig(ctx)
	if err != nil {
		return OAuthResult{}, err
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return OAuthResult{}, ErrGitHubNotReady
	}
	now := s.now()
	ts := now.UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthResult{}, err
	}
	defer tx.Rollback()

	var boundUserID, verifier, storedRedirect, mode string
	var expiresAt int64
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, code_verifier, redirect_uri, mode, expires_at FROM oauth_states WHERE state_hash=?`,
		HashToken(state),
	).Scan(&boundUserID, &verifier, &storedRedirect, &mode, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthResult{}, fmt.Errorf("%w: invalid or already used oauth state", ErrUnauthorized)
	}
	if err != nil {
		return OAuthResult{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM oauth_states WHERE state_hash=?", HashToken(state)); err != nil {
		return OAuthResult{}, err
	}
	if ts >= expiresAt {
		_ = tx.Commit()
		return OAuthResult{}, fmt.Errorf("%w: oauth state expired", ErrUnauthorized)
	}
	if redirectURI != "" && redirectURI != storedRedirect {
		_ = tx.Commit()
		return OAuthResult{}, fmt.Errorf("%w: redirect_uri mismatch", ErrBadInput)
	}
	if mode == "connect" && callerUserID != "" && boundUserID != "" && callerUserID != boundUserID {
		_ = tx.Commit()
		return OAuthResult{}, ErrForbidden
	}
	if err := tx.Commit(); err != nil {
		return OAuthResult{}, err
	}

	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {storedRedirect},
		"code_verifier": {verifier},
	}
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.OAuthBaseURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthResult{}, err
	}
	tokenReq.Header.Set("Accept", "application/json")
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResp, err := httpClient.Do(tokenReq)
	if err != nil {
		return OAuthResult{}, err
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode < 200 || tokenResp.StatusCode >= 300 {
		return OAuthResult{}, fmt.Errorf("github token exchange returned HTTP %d", tokenResp.StatusCode)
	}
	var tokenData struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(tokenResp.Body, 1<<20)).Decode(&tokenData); err != nil {
		return OAuthResult{}, err
	}
	if tokenData.Error != "" || tokenData.AccessToken == "" {
		return OAuthResult{}, fmt.Errorf("%w: github rejected authorization code", ErrUnauthorized)
	}

	userReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.APIBaseURL+"/user", nil)
	if err != nil {
		return OAuthResult{}, err
	}
	userReq.Header.Set("Authorization", "Bearer "+tokenData.AccessToken)
	userReq.Header.Set("Accept", "application/vnd.github+json")
	userReq.Header.Set("User-Agent", "kamui-cloud")
	userResp, err := httpClient.Do(userReq)
	if err != nil {
		return OAuthResult{}, err
	}
	defer userResp.Body.Close()
	if userResp.StatusCode != http.StatusOK {
		return OAuthResult{}, fmt.Errorf("github user profile returned HTTP %d", userResp.StatusCode)
	}
	var ghUser struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(userResp.Body, 1<<20)).Decode(&ghUser); err != nil || ghUser.ID == 0 || ghUser.Login == "" {
		return OAuthResult{}, errors.New("invalid github user profile")
	}

	encAccess, err := Seal(s.encKey, tokenData.AccessToken)
	if err != nil {
		return OAuthResult{}, err
	}
	encRefresh := ""
	if tokenData.RefreshToken != "" {
		encRefresh, err = Seal(s.encKey, tokenData.RefreshToken)
		if err != nil {
			return OAuthResult{}, err
		}
	}

	targetUserID := boundUserID
	if targetUserID == "" {
		targetUserID = callerUserID
	}
	if targetUserID == "" {
		// GitHub sign-in: look up user previously linked to this github_user_id.
		err = s.db.QueryRowContext(ctx, "SELECT user_id FROM github_connections WHERE github_user_id=?", ghUser.ID).Scan(&targetUserID)
		if errors.Is(err, sql.ErrNoRows) {
			settings, sErr := s.GetSettings(ctx)
			if sErr != nil {
				return OAuthResult{}, sErr
			}
			if settings.RegistrationPolicy == RegistrationDisabled || settings.RegistrationPolicy == RegistrationOwnerOnly {
				return OAuthResult{}, ErrRegistrationOff
			}
			randPass, pErr := NewSecretToken("ghp_", 24)
			if pErr != nil {
				return OAuthResult{}, pErr
			}
			uname := "gh-" + strings.ToLower(ghUser.Login)
			if len(uname) > 64 {
				uname = uname[:64]
			}
			created, _, cErr := s.CreateUser(ctx, "github-oauth", CreateUserInput{
				Username:    uname,
				Email:       ghUser.Email,
				DisplayName: fallbackStr(ghUser.Name, ghUser.Login),
				Password:    randPass,
			}, true)
			if cErr != nil {
				return OAuthResult{}, cErr
			}
			targetUserID = created.ID
		} else if err != nil {
			return OAuthResult{}, err
		}
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO github_connections(user_id, github_user_id, github_login, token_encrypted, refresh_token_encrypted, scopes, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET
		   github_user_id=excluded.github_user_id,
		   github_login=excluded.github_login,
		   token_encrypted=excluded.token_encrypted,
		   refresh_token_encrypted=excluded.refresh_token_encrypted,
		   scopes=excluded.scopes,
		   updated_at=excluded.updated_at`,
		targetUserID, ghUser.ID, ghUser.Login, encAccess, encRefresh, tokenData.Scope, ts, ts,
	)
	if err != nil {
		return OAuthResult{}, err
	}
	_ = s.RecordAudit(ctx, targetUserID, "", "github.connect", "user", targetUserID, "success", ghUser.Login)
	user, err := s.GetUser(ctx, targetUserID)
	if err != nil {
		return OAuthResult{}, err
	}
	status, err := s.GetGitHubStatus(ctx, targetUserID)
	if err != nil {
		return OAuthResult{}, err
	}
	return OAuthResult{Mode: mode, User: user, GitHub: status}, nil
}

func (s *Store) GetGitHubStatus(ctx context.Context, userID string) (GitHubStatus, error) {
	cfg, err := s.GetGitHubConfig(ctx)
	if err != nil {
		return GitHubStatus{}, err
	}
	out := GitHubStatus{Configured: cfg.ClientID != "" && cfg.ClientSecret != ""}
	var login, scopes string
	var updated int64
	err = s.db.QueryRowContext(ctx, "SELECT github_login, scopes, updated_at FROM github_connections WHERE user_id=?", userID).Scan(&login, &scopes, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	t := time.Unix(0, updated).UTC()
	out.Connected = true
	out.Login = login
	out.Scopes = scopes
	out.UpdatedAt = &t
	return out, nil
}

func (s *Store) DisconnectGitHub(ctx context.Context, userID string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM github_connections WHERE user_id=?", userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_ = s.RecordAudit(ctx, userID, "", "github.disconnect", "user", userID, "success", "")
	}
	return nil
}

func (s *Store) GetUserGitHubToken(ctx context.Context, userID string) (string, error) {
	var enc string
	err := s.db.QueryRowContext(ctx, "SELECT token_encrypted FROM github_connections WHERE user_id=?", userID).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrGitHubNotLinked
	}
	if err != nil {
		return "", err
	}
	return Open(s.encKey, enc)
}

func (s *Store) ListUserRepos(ctx context.Context, httpClient *http.Client, userID string) ([]GitHubRepo, error) {
	token, err := s.GetUserGitHubToken(ctx, userID)
	if err != nil {
		return nil, err
	}
	cfg, err := s.GetGitHubConfig(ctx)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.APIBaseURL+"/user/repos?per_page=100&sort=updated&affiliation=owner,collaborator,organization_member", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kamui-cloud")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github repositories returned HTTP %d", resp.StatusCode)
	}
	var repos []GitHubRepo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&repos); err != nil {
		return nil, err
	}
	return repos, nil
}
