package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSubmissionTLSAuthAndArgumentVector(t *testing.T) {
	const token = "valid-test-token-12345678901234567890"
	t.Setenv("KAMUI_CONFIG", filepath.Join(t.TempDir(), "cli-config.json"))
	t.Setenv("KAMUI_TOKEN", token)
	t.Setenv("KAMUI_TOKEN_FILE", "")
	t.Setenv("KAMUI_CA_FILE", "")
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/workloads" ||
			r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Idempotency-Key") != "test-key" {
			t.Errorf("unexpected submission method, path or headers")
		}
		var input workloadSubmission
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Kind != "job" ||
			input.Image != "busybox:1.37" || input.Resources.CPUMillis != 500 ||
			!slices.Equal(input.Args, []string{"printf", "--", "two words", "$HOME"}) {
			t.Errorf("submission payload = %+v, %v", input, err)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"test"}`))
	}))
	defer server.Close()

	ca := filepath.Join(t.TempDir(), "ca.pem")
	err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	if err != nil {
		t.Fatal(err)
	}
	run := func(endpoint string, caArgs ...string) (int, string, string) {
		var out, stderr bytes.Buffer
		args := append([]string{"--url", endpoint, "--json"}, caArgs...)
		args = append(args, "job", "submit", "--idempotency-key", "test-key", "--cpu-millis", "500",
			"busybox:1.37", "--", "printf", "--", "two words", "$HOME")
		code := execute(context.Background(), args, nil, &out, &stderr)
		return code, out.String(), stderr.String()
	}
	if code, out, stderr := run(server.URL); code != 6 || out != "" || !strings.Contains(stderr, "tls_verification") {
		t.Fatalf("untrusted CA: exit %d, output %q, stderr %q", code, out, stderr)
	}
	mismatch, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	mismatch.Host = "localhost:" + mismatch.Port()
	if code, out, stderr := run(mismatch.String(), "--ca-file", ca); code != 6 || out != "" || !strings.Contains(stderr, "tls_verification") {
		t.Fatalf("wrong hostname: exit %d, output %q, stderr %q", code, out, stderr)
	}
	if code, out, stderr := run(server.URL, "--ca-file", ca); code != 0 || out != "{\"id\":\"test\"}\n" || stderr != "" || calls.Load() != 1 {
		t.Fatalf("trusted TLS: exit %d, output %q, stderr %q, calls %d", code, out, stderr, calls.Load())
	}
}

func TestClientDoesNotFollowRedirectsAndMapsAuthErrors(t *testing.T) {
	t.Setenv("KAMUI_CONFIG", filepath.Join(t.TempDir(), "cli-config.json"))
	t.Setenv("KAMUI_TOKEN", "valid-test-token-12345678901234567890")
	t.Setenv("KAMUI_TOKEN_FILE", "")
	t.Setenv("KAMUI_CA_FILE", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case workspacesPath:
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
		case "/v1/workers/local":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"access denied"}}`))
		default:
			t.Error("unexpected redirected request")
		}
	}))
	defer server.Close()
	run := func(args ...string) (int, string) {
		var out, stderr bytes.Buffer
		code := execute(context.Background(), append([]string{"--url", server.URL, "--json"}, args...), nil, &out, &stderr)
		if out.Len() != 0 {
			t.Errorf("error printed to stdout: %q", out.String())
		}
		return code, stderr.String()
	}
	if code, stderr := run("workspace", "list"); code != 1 || !strings.Contains(stderr, `"status":307`) {
		t.Fatalf("redirect = %d, %s", code, stderr)
	}
	if code, stderr := run("device", "inspect"); code != 3 || !strings.Contains(stderr, `"code":"unauthorized"`) {
		t.Fatalf("unauthorized = %d, %s", code, stderr)
	}
	if _, err := endpointURL("http://example.com"); err == nil {
		t.Fatal("external cleartext endpoint was accepted")
	}
}
