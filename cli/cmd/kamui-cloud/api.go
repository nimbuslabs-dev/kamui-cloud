package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
)

type apiClient struct {
	url, token string
	http       *http.Client
}

func endpointURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.ForceQuery ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, failure(2, "invalid_url", "provide a coordinator URL with no credentials, path, query or fragment")
	}
	if parsed.Scheme == "https" {
		return parsed, nil
	}
	ip, err := netip.ParseAddr(parsed.Hostname())
	if parsed.Scheme == "http" && err == nil && ip.Zone() == "" && ip.IsLoopback() {
		return parsed, nil
	}
	return nil, failure(2, "invalid_url", "HTTPS is required except for an explicit loopback IP in native mode")
}

func (a *app) client() (*apiClient, error) {
	endpoint, ca, envPath := a.url, a.ca, ""
	if endpoint == "" {
		local, localCA, localEnv, err := localEndpoint(a.cloud.Root)
		if err != nil {
			return nil, failure(2, "config", "cannot resolve the local stack; set --root or --url")
		}
		endpoint, envPath = local, localEnv
		if ca == "" {
			ca = localCA
		}
	}
	u, err := endpointURL(endpoint)
	if err != nil {
		return nil, err
	}
	token := os.Getenv("KAMUI_TOKEN")
	if a.tokenFile != "" {
		data, err := os.ReadFile(a.tokenFile)
		if err != nil {
			return nil, failure(2, "credentials", "cannot read the coordinator token file")
		}
		token = strings.TrimSpace(string(data))
	} else if token == "" && envPath != "" {
		token, err = localToken(envPath)
		if err != nil {
			return nil, failure(2, "credentials", "cannot read the local coordinator token; run cloud init")
		}
	}
	if len(token) < 32 || strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return nil, failure(2, "credentials", "set a valid token using the local stack, KAMUI_TOKEN or --token-file")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != "" {
		if u.Scheme != "https" {
			return nil, failure(2, "config", "--ca-file requires HTTPS")
		}
		data, err := os.ReadFile(ca)
		if err != nil {
			return nil, failure(2, "tls_ca", "cannot read the coordinator CA certificate")
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(data) {
			return nil, failure(2, "tls_ca", "the coordinator CA file contains no valid PEM certificate")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = tlsConfig
	return &apiClient{
		url: strings.TrimRight(u.String(), "/"), token: token,
		http: &http.Client{
			Transport:     transport,
			Timeout:       165 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *apiClient) request(ctx context.Context, method, path string, body any, key string) (json.RawMessage, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var verification *tls.CertificateVerificationError
		if errors.As(err, &verification) {
			return nil, failure(6, "tls_verification", "coordinator certificate verification failed; check the CA and hostname")
		}
		return nil, failure(6, "connection", "could not reach the coordinator")
	}
	defer resp.Body.Close()

	const limit = 8 << 20
	result, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(result) > limit {
		return nil, failure(6, "response", "cannot read the coordinator response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.responseError(resp.StatusCode, result)
	}
	if resp.StatusCode == http.StatusNoContent {
		return json.RawMessage(`{"ok":true}`), nil
	}
	if !json.Valid(result) {
		return nil, failure(6, "response", "the coordinator returned invalid JSON")
	}
	return result, nil
}

func (c *apiClient) responseError(status int, data []byte) error {
	detail := &cliError{Status: status, Code: "http_error", Message: http.StatusText(status), exit: 1}
	var reply struct {
		Error struct{ Code, Message string }
	}
	if json.Unmarshal(data, &reply) == nil && reply.Error.Code != "" && reply.Error.Message != "" {
		detail.Code = strings.ReplaceAll(reply.Error.Code, c.token, "[redacted]")
		detail.Message = strings.ReplaceAll(reply.Error.Message, c.token, "[redacted]")
	}
	switch status {
	case 400, 422:
		detail.exit = 2
	case 401, 403:
		detail.exit = 3
	case 404:
		detail.exit = 4
	case 409, 429:
		detail.exit = 5
	default:
		if status >= 500 {
			detail.exit = 6
		}
	}
	return detail
}

func (a *app) send(cmd *cobra.Command, method, path string, body any, key string) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	result, err := client.request(cmd.Context(), method, path, body, key)
	if err != nil {
		return err
	}
	return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
}

func (a *app) healthCommand() *cobra.Command {
	return &cobra.Command{
		Use: "health", Short: "Check coordinator connectivity and local worker readiness", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			if _, err := client.request(cmd.Context(), "GET", "/healthz", nil, ""); err != nil {
				return err
			}
			result, err := client.request(cmd.Context(), "GET", "/v1/workers/local", nil, "")
			if err != nil {
				return err
			}
			var worker struct{ Health string }
			if err := json.Unmarshal(result, &worker); err != nil {
				return failure(6, "response", "invalid worker health response")
			}
			if worker.Health != "healthy" {
				return failure(6, "worker_unhealthy", "coordinator is reachable but the local worker is unavailable")
			}
			return printJSON(cmd.OutOrStdout(), map[string]any{"status": "healthy", "worker": result}, a.cloud.JSON)
		},
	}
}

func (a *app) deviceCommand() *cobra.Command {
	group := &cobra.Command{Use: "device", Short: "Inspect registered hardware"}
	group.AddCommand(&cobra.Command{
		Use: "inspect", Short: "Show local worker capabilities", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.send(cmd, "GET", "/v1/workers/local", nil, "")
		},
	})
	return group
}

// Startup verifies the local stack even when the CLI has a separate API endpoint configured.
func (a *app) verifyLocalStart(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	local := &app{cloud: a.cloud}
	client, err := local.client()
	if err != nil {
		return err
	}
	_, err = client.request(ctx, "GET", "/v1/workers/local", nil, "")
	return err
}
