package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const path = "/workspace/ws-01234567/socket?reconnectionToken=fixture"

func TestEstablishedTunnelClosesWithSessionLease(t *testing.T) {
	for _, reason := range []string{"revocation", "silent authorization outage"} {
		t.Run(reason, func(t *testing.T) {
			revoked := make(chan struct{})
			auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RequestURI() != "/api/auth/ide?watch=1" || r.Host != "kamui.example" ||
					r.Header.Get("Origin") != "https://kamui.example" || r.Header.Get("Cookie") != "session=fixture" ||
					r.Header.Get("X-Kamui-Upgrade") != "websocket" || r.Header.Get("X-Forwarded-Proto") != "https" {
					t.Error("incorrect upstream auth request")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, _ = io.WriteString(w, "1")
				w.(http.Flusher).Flush()
				select {
				case <-revoked:
				case <-r.Context().Done():
				}
			}))
			defer auth.Close()

			closed := make(chan struct{})
			ide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.RequestURI(); got != "/socket?reconnectionToken=fixture" {
					t.Errorf("workspace URI = %s", got)
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-CSRF-Token") != "" {
					t.Error("Console credentials reached workspace")
				}
				conn, rw, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer close(closed)
				defer conn.Close()
				fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
				_ = rw.Flush()
				_, _ = io.Copy(io.Discard, rw)
			}))
			defer ide.Close()
			timeout := 5 * time.Second
			if reason == "silent authorization outage" {
				timeout = time.Second
			}
			tunnel := httptest.NewServer(&ideTunnel{
				console: auth.URL + "/api/auth/ide?watch=1", auth: auth.Client(), transport: ide.Client().Transport,
				workspace: func(string) string { return strings.TrimPrefix(ide.URL, "http://") }, timeout: timeout,
			})
			defer tunnel.Close()

			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(tunnel.URL, "http://"), 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
			fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: kamui.example\r\nOrigin: https://kamui.example\r\nX-Forwarded-Proto: https\r\nCookie: session=fixture\r\nAuthorization: Bearer fixture\r\nX-CSRF-Token: fixture\r\nConnection: Upgrade\r\nUpgrade: WebSocket\r\n\r\n", path)
			reader := bufio.NewReader(conn)
			response, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("upgrade returned %v", response.Status)
			}
			if reason == "revocation" {
				close(revoked)
			}
			if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
				t.Fatalf("expected closed downstream socket, got %v", err)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("upstream socket remained open")
			}
		})
	}
}

func TestTunnelFailsClosedBeforeWorkspaceContact(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = io.WriteString(w, "invalid lease")
				}
			}))
			defer auth.Close()
			tunnel := &ideTunnel{
				console: auth.URL, auth: auth.Client(), workspace: func(string) string {
					t.Fatal("workspace contacted without valid auth lease")
					return ""
				}, timeout: time.Second,
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("Origin", "https://kamui.example")
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			response := httptest.NewRecorder()
			tunnel.ServeHTTP(response, req)
			expected := status
			if status == http.StatusServiceUnavailable || status == http.StatusOK {
				expected = http.StatusBadGateway
			}
			if response.Code != expected {
				t.Fatalf("HTTP %d, want %d", response.Code, expected)
			}
		})
	}
}
