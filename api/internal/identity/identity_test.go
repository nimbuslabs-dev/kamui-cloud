package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
)

func openTestIdentity(t *testing.T) (*core.Store, *Store) {
	t.Helper()
	ctx := context.Background()
	cs, err := core.Open(ctx, filepath.Join(t.TempDir(), "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	id := NewStore(cs.DB(), "master-test-secret-0123456789abcdef", "admin-password-0123456789abcdef")
	if err := id.EnsureAdminBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	return cs, id
}

func TestIdentityUsersRolesInvitesSessionsAndAudit(t *testing.T) {
	ctx := context.Background()
	_, s := openTestIdentity(t)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	// Admin bootstrap authentication works offline.
	admin, err := s.AuthenticateCredentials(ctx, "admin", "admin-password-0123456789abcdef", "127.0.0.1")
	if err != nil || admin.ID != LocalAdminID || !admin.IsSystemAdmin {
		t.Fatalf("admin auth failed: %+v, %v", admin, err)
	}

	// Create two independent local users.
	alice, aliceProj, err := s.CreateUser(ctx, LocalAdminID, CreateUserInput{
		Username: "alice", Email: "alice@example.com", Password: "alice-password-123",
	}, true)
	if err != nil || aliceProj == nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, bobProj, err := s.CreateUser(ctx, LocalAdminID, CreateUserInput{
		Username: "bob", Email: "bob@example.com", Password: "bob-password-123",
	}, true)
	if err != nil || bobProj == nil {
		t.Fatalf("create bob: %v", err)
	}

	// Personal mode blocks invitations.
	if _, err := s.CreateInvitation(ctx, alice.ID, aliceProj.ID, "bob@example.com", "", RoleDeveloper, time.Hour); !errors.Is(err, ErrPersonalMode) {
		t.Fatalf("expected ErrPersonalMode, got %v", err)
	}
	// Switch to collaborative mode.
	if _, err := s.UpdateSettings(ctx, LocalAdminID, Settings{CloudMode: CloudModeCollaborative}); err != nil {
		t.Fatal(err)
	}

	// Alice invites Bob with 60s expiry; test expiry.
	expiredInv, err := s.CreateInvitation(ctx, alice.ID, aliceProj.ID, "", "bob", RoleViewer, 30*time.Second)
	if err != nil || expiredInv.Token == "" {
		t.Fatalf("create expired invite: %v", err)
	}
	now = now.Add(time.Minute)
	bobPrincipal := &Principal{User: bob}
	if _, _, err := s.AcceptInvitation(ctx, bobPrincipal, AcceptInvitationInput{Token: expiredInv.Token}); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("expected ErrInviteExpired, got %v", err)
	}

	// Valid single-use invite.
	inv, err := s.CreateInvitation(ctx, alice.ID, aliceProj.ID, "", "bob", RoleDeveloper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pm, _, err := s.AcceptInvitation(ctx, bobPrincipal, AcceptInvitationInput{Token: inv.Token})
	if err != nil || pm.Role != RoleDeveloper || pm.ID != aliceProj.ID {
		t.Fatalf("accept invite: %+v, %v", pm, err)
	}
	// Second use is rejected.
	if _, _, err := s.AcceptInvitation(ctx, bobPrincipal, AcceptInvitationInput{Token: inv.Token}); !errors.Is(err, ErrInviteUsed) {
		t.Fatalf("expected ErrInviteUsed on replay, got %v", err)
	}

	// Revoked invite is rejected.
	revInv, err := s.CreateInvitation(ctx, alice.ID, aliceProj.ID, "carol@example.com", "", RoleViewer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvitation(ctx, alice.ID, aliceProj.ID, revInv.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvitation(ctx, nil, AcceptInvitationInput{Token: revInv.Token, Username: "carol", Password: "carol-password-123"}); !errors.Is(err, ErrInviteRevoked) {
		t.Fatalf("expected ErrInviteRevoked, got %v", err)
	}

	// Session issuance and revocation.
	sess, tok, err := s.IssueSession(ctx, bob.ID, SessionCLI, "laptop", "", aliceProj.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.AuthenticateToken(ctx, tok)
	if err != nil || p.User.ID != bob.ID {
		t.Fatalf("authenticate token: %+v, %v", p, err)
	}
	if err := s.RevokeSessionByID(ctx, bob.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateToken(ctx, tok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized after revocation, got %v", err)
	}

	events, err := s.ListAuditEvents(ctx, "", 20)
	if err != nil || len(events) < 3 {
		t.Fatalf("expected audit events, got %d (%v)", len(events), err)
	}
}

func TestPerUserGitHubOAuthAndIsolation(t *testing.T) {
	ctx := context.Background()
	_, s := openTestIdentity(t)

	ghMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			_ = r.ParseForm()
			code := r.Form.Get("code")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + code, "scope": "repo read:user"})
		case "/user":
			auth := r.Header.Get("Authorization")
			if auth == "Bearer tok-alice-code" {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 101, "login": "alice-gh"})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 202, "login": "bob-gh"})
			}
		case "/user/repos":
			auth := r.Header.Get("Authorization")
			if auth == "Bearer tok-alice-code" {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "full_name": "alice-gh/secret", "private": true, "clone_url": "https://github.com/alice-gh/secret.git"}})
			} else {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 2, "full_name": "bob-gh/repo", "private": false, "clone_url": "https://github.com/bob-gh/repo.git"}})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ghMock.Close()

	if err := s.SaveGitHubConfig(ctx, LocalAdminID, GitHubConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		CallbackURL:  "https://127.0.0.1:8081/api/github/callback",
		OAuthBaseURL: ghMock.URL,
		APIBaseURL:   ghMock.URL,
	}); err != nil {
		t.Fatal(err)
	}
	alice, _, _ := s.CreateUser(ctx, LocalAdminID, CreateUserInput{Username: "alice", Password: "password-12345"}, true)
	bob, _, _ := s.CreateUser(ctx, LocalAdminID, CreateUserInput{Username: "bob", Password: "password-12345"}, true)

	startA, err := s.StartGitHubOAuth(ctx, alice.ID, "connect", "")
	if err != nil {
		t.Fatal(err)
	}
	// Wrong state must fail.
	if _, err := s.CompleteGitHubOAuth(ctx, ghMock.Client(), alice.ID, "alice-code", "bad-state", ""); err == nil {
		t.Fatal("expected invalid state error")
	}
	// Bob cannot complete Alice's state.
	if _, err := s.CompleteGitHubOAuth(ctx, ghMock.Client(), bob.ID, "alice-code", startA.State, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for mismatched user, got %v", err)
	}
	// Replay/legitimate flow with fresh state.
	startA2, _ := s.StartGitHubOAuth(ctx, alice.ID, "connect", "")
	resA, err := s.CompleteGitHubOAuth(ctx, ghMock.Client(), alice.ID, "alice-code", startA2.State, "")
	if err != nil || !resA.GitHub.Connected || resA.GitHub.Login != "alice-gh" {
		t.Fatalf("complete oauth alice: %+v, %v", resA, err)
	}
	// Bob has no GitHub connection yet!
	if _, err := s.GetUserGitHubToken(ctx, bob.ID); !errors.Is(err, ErrGitHubNotLinked) {
		t.Fatalf("expected ErrGitHubNotLinked for bob, got %v", err)
	}
	repos, err := s.ListUserRepos(ctx, ghMock.Client(), alice.ID)
	if err != nil || len(repos) != 1 || repos[0].FullName != "alice-gh/secret" {
		t.Fatalf("alice repos = %+v, %v", repos, err)
	}
	if err := s.DisconnectGitHub(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUserGitHubToken(ctx, alice.ID); !errors.Is(err, ErrGitHubNotLinked) {
		t.Fatalf("expected disconnected after revoke, got %v", err)
	}
}
