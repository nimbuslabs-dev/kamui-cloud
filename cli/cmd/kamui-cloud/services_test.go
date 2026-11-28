package main

import (
	"io"
	"net/http"
	"testing"
)

func TestBackupRestoreExportMode(t *testing.T) {
	const id = "bck-0123456789abcdef01234567"
	_, endpoint, token := testCoordinator(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/backups/"+id+"/restore" ||
			r.URL.Query().Get("project_id") != "default" || r.URL.Query().Get("mode") != "export" {
			t.Errorf("restore request = %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"restored_to":"restores/example/data"}`)
	})
	code, out := runCLI(t, "--url", endpoint, "--token-file", token,
		"backup", "restore", id, "--project", "default", "--export")
	if code != 0 {
		t.Fatalf("backup restore --export = %d %s", code, out)
	}
}

func TestStoreDownloadEscapesObjectKey(t *testing.T) {
	const fixture = "restored object bytes\n"
	_, endpoint, token := testCoordinator(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.RequestURI != "/v1/projects/default/buckets/restored/objects/nested/my%20object%3F%23%25.txt" {
			t.Errorf("download request = %s %s", r.Method, r.RequestURI)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, fixture)
	})
	code, out := runCLI(t, "--url", endpoint, "--token-file", token,
		"store", "object", "download", "restored", "nested/my object?#%.txt", "--project", "default")
	if code != 0 || out != fixture {
		t.Fatalf("store object download = %d %q", code, out)
	}
}
