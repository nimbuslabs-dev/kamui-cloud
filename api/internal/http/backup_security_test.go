package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/backup"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/objectstore"
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
	backups.UseObjectStore(objectstore.NewStore(coreStore.DB(), &objectstore.LocalBackend{Root: root}, nil, nil))
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
	restore := call(bob, "POST", "/v1/backups/"+bobBackup.ID+"/restore?project_id="+bobProject, bobBackup.ID, handler.RestoreBackup)
	var op backup.Restore
	if err := json.Unmarshal(restore.Body.Bytes(), &op); err != nil || restore.Code != http.StatusOK || op.Status != "succeeded" || restore.Header().Get("X-Kamui-Restore-ID") != op.ID {
		t.Fatalf("owner restore = %d: %s, %v", restore.Code, restore.Body.String(), err)
	}
	mux := http.NewServeMux()
	handler.register(mux)
	for _, tc := range []struct {
		principal identity.Principal
		project   string
		want      int
	}{
		{bob, bobProject, http.StatusOK},
		{alice, aliceProject, http.StatusNotFound},
		{alice, bobProject, http.StatusForbidden},
	} {
		for _, path := range []string{"/v1/backup-restores/" + op.ID, "/v1/backups/" + bobBackup.ID + "/restores"} {
			got := call(tc.principal, "GET", path+"?project_id="+tc.project, "", mux.ServeHTTP)
			if got.Code != tc.want {
				t.Errorf("%s as %s = %d, want %d", path, tc.principal.User.ID, got.Code, tc.want)
			}
		}
	}
	export := call(bob, "POST", "/v1/backups/"+bobBackup.ID+"/restore?mode=export&project_id="+bobProject, bobBackup.ID, handler.RestoreBackup)
	if export.Code != http.StatusOK || !strings.Contains(export.Body.String(), `"restored_to":"restores/restore-`) {
		t.Fatalf("owner export = %d: %s", export.Code, export.Body.String())
	}
}
