// ide-tunnel keeps an IDE WebSocket attached to its Console session.
package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/workspaceinput"
)

const leaseTimeout = 15 * time.Second

func internalTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		DisableCompression:    true,
		ResponseHeaderTimeout: 5 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
}

func main() {
	auth := &http.Client{
		Transport: internalTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	proxy := &ideTunnel{
		console:   "http://console:3000/api/auth/ide?watch=1",
		auth:      auth,
		transport: internalTransport(),
		workspace: func(id string) string { return "kamui-workspace-" + id + ":8080" },
		timeout:   leaseTimeout,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /workspace/", proxy)
	server := &http.Server{Addr: ":8083", Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	log.Fatal(server.ListenAndServe())
}

type ideTunnel struct {
	console   string
	auth      *http.Client
	transport http.RoundTripper
	workspace func(string) string
	timeout   time.Duration
}

func (t *ideTunnel) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id, _, hasPath := strings.Cut(strings.TrimPrefix(req.URL.Path, "/workspace/"), "/")
	prefix := "/workspace/" + id
	path, plainPrefix := strings.CutPrefix(req.URL.EscapedPath(), prefix)
	if !workspaceinput.ValidID(id) || !hasPath || !plainPrefix || !strings.HasPrefix(path, "/") {
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodGet || !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") ||
		!strings.EqualFold(req.Header.Get("Connection"), "upgrade") || req.ContentLength != 0 || len(req.TransferEncoding) != 0 {
		http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}
	// Console validates the exact public origin with its session.
	if req.Header.Get("X-Forwarded-Proto") != "https" || req.Header.Get("Origin") == "" {
		http.Error(w, "Invalid request origin", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	timer := time.AfterFunc(t.timeout, cancel)
	defer timer.Stop()
	leaseReq, err := http.NewRequestWithContext(ctx, http.MethodGet, t.console, nil)
	if err != nil {
		unavailable(w)
		return
	}
	leaseReq.Host = req.Host
	leaseReq.Header.Set("Cookie", req.Header.Get("Cookie"))
	leaseReq.Header.Set("Origin", req.Header.Get("Origin"))
	leaseReq.Header.Set("X-Forwarded-Proto", "https")
	leaseReq.Header.Set("X-Kamui-Upgrade", "websocket")
	lease, err := t.auth.Do(leaseReq)
	if err != nil {
		unavailable(w)
		return
	}
	defer lease.Body.Close()
	switch lease.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		http.Error(w, "IDE authorization required", lease.StatusCode)
		return
	case http.StatusOK:
	default:
		unavailable(w)
		return
	}

	var heartbeat [1]byte
	readHeartbeat := func() bool {
		_, err := io.ReadFull(lease.Body, heartbeat[:])
		if err != nil || heartbeat[0] != '1' {
			return false
		}
		timer.Reset(t.timeout)
		return ctx.Err() == nil
	}
	if !readHeartbeat() {
		unavailable(w)
		return
	}
	go func() {
		defer cancel()
		for readHeartbeat() {
		}
	}()
	target := t.workspace(id)
	proxy := httputil.ReverseProxy{
		Transport: t.transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme, p.Out.URL.Host = "http", target
			p.Out.URL.Path = strings.TrimPrefix(p.In.URL.Path, prefix)
			p.Out.URL.RawPath = path
			p.Out.Host = p.In.Host
			// The workspace receives the WebSocket handshake without Console credentials.
			headers := make(http.Header)
			for _, name := range []string{"Connection", "Upgrade", "Origin", "Sec-WebSocket-Key",
				"Sec-WebSocket-Version", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions", "User-Agent"} {
				if values := p.Out.Header.Values(name); len(values) > 0 {
					headers[http.CanonicalHeaderKey(name)] = values
				}
			}
			headers.Set("X-Forwarded-Proto", "https")
			headers.Set("X-Forwarded-Host", p.In.Host)
			p.Out.Header = headers
		},
		ModifyResponse: func(response *http.Response) error {
			for _, name := range []string{"Set-Cookie", "Clear-Site-Data", "Service-Worker-Allowed", "X-Accel-Redirect"} {
				response.Header.Del(name)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { unavailable(w) },
	}
	// ReverseProxy cancels the upstream side. Close the hijacked downstream
	// connection too, so a blocked downstream write cannot delay revocation.
	downstream := &revocableWriter{ResponseWriter: w, ctx: ctx}
	defer func() {
		if downstream.stop != nil {
			downstream.stop()
		}
	}()
	proxy.ServeHTTP(downstream, req.WithContext(ctx))
}

func unavailable(w http.ResponseWriter) {
	http.Error(w, "IDE unavailable", http.StatusBadGateway)
}

type revocableWriter struct {
	http.ResponseWriter
	ctx  context.Context
	stop func() bool
}

func (w *revocableWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *revocableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.stop = context.AfterFunc(w.ctx, func() { _ = conn.Close() })
	}
	return conn, buffer, err
}
