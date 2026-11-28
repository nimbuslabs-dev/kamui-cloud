package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/backup"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

func TestBackupProjectIsolation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	coreStore, err := core.Open(ctx, filepath.Join(root, "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer coreStore.Close()
	ids := identity.NewStore(coreStore.DB(), "test-secret-0123456789abcdef", "test-password-0123456789abcdef")
	makeUser := func(name string) (identity.Principal, string) {
		t.Helper()
		user, project, err := ids.CreateUser(ctx, identity.LocalAdminID, identity.CreateUserInput{
			Username: name, Email: name + "@example.test", Password: "test-password-0123456789abcdef",
		}, true)
		if err != nil {
			t.Fatal(err)
		}
		return identity.Principal{User: user}, project.ID
	}
	alice, aliceProject := makeUser("alice")
	bob, bobProject := makeUser("bob")
	backups := backup.NewStore(coreStore.DB(), ids, root, nil)
	bobBackup, err := backups.CreateObjectBackup(ctx, bobProject, bob.User.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Seed metadata separately so authorization checks do not depend on snapshot creation.
	if _, err := coreStore.DB().Exec("INSERT INTO backups(id,project_id,kind,target,status,created_by,created_at) VALUES('bck-metadata','','metadata','coordinator','succeeded',?,0)", identity.LocalAdminID); err != nil {
		t.Fatal(err)
	}
	handler := &servicesHandler{ids: ids, backups: backups}
	call := func(p identity.Principal, method, path, id string, h http.HandlerFunc) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.SetPathValue("bck", id)
		w := httptest.NewRecorder()
		h(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		return w
	}
	for _, tc := range []struct {
		method  string
		handler http.HandlerFunc
	}{
		{"GET", handler.GetBackup}, {"GET", handler.DownloadBackup}, {"POST", handler.RestoreBackup},
	} {
		path := "/v1/backups/" + bobBackup.ID + "?project_id=" + aliceProject
		if got := call(alice, tc.method, path, bobBackup.ID, tc.handler).Code; got != http.StatusNotFound {
			t.Errorf("cross-project %s = %d, want 404", tc.method, got)
		}
		if got := call(alice, tc.method, "/v1/backups/bck-metadata", "bck-metadata", tc.handler).Code; got != http.StatusForbidden {
			t.Errorf("metadata %s for non-admin = %d, want 403", tc.method, got)
		}
	}
	if got := call(bob, "GET", "/v1/backups/"+bobBackup.ID+"?project_id="+bobProject, bobBackup.ID, handler.GetBackup).Code; got != http.StatusOK {
		t.Errorf("owner inspection = %d, want 200", got)
	}
	list := call(alice, "GET", "/v1/backups?project_id="+aliceProject, "", handler.ListBackups)
	if list.Code != http.StatusOK || strings.TrimSpace(list.Body.String()) != "[]" {
		t.Errorf("project list = %d: %s", list.Code, list.Body.String())
	}
}
