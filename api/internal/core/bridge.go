package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
)

type bridgeInput struct {
	Ref  ExecutionRef  `json:"ref"`
	Spec ContainerSpec `json:"spec"`
}

// BridgeHandler runs on the private Workspace Manager network, behind its bearer middleware.
func BridgeHandler(executor Executor) http.Handler {
	mux := http.NewServeMux()
	if volumeBackend, ok := executor.(bridgeVolumeBackend); ok {
		registerVolumeBridge(mux, volumeBackend)
	}
	mux.HandleFunc("POST /internal/core/docker/{operation}", func(w http.ResponseWriter, r *http.Request) {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		d.DisallowUnknownFields()
		var input bridgeInput
		if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid execution request")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		var (
			result any = struct{}{}
			err    error
		)
		switch r.PathValue("operation") {
		case "capacity":
			result, err = executor.Capacity(ctx)
		case "create":
			err = executor.Create(ctx, input.Ref, input.Spec)
		case "start":
			err = executor.Start(ctx, input.Ref)
		case "inspect":
			result, err = executor.Inspect(ctx, input.Ref)
		case "stop":
			err = executor.Stop(ctx, input.Ref)
		case "remove":
			err = executor.Remove(ctx, input.Ref)
		case "logs":
			result, err = executor.Logs(ctx, input.Ref)
		default:
			apierr.WriteError(w, http.StatusNotFound, "not_found", "unknown execution operation")
			return
		}
		switch {
		case err == nil:
			apierr.WriteJSON(w, http.StatusOK, result)
		case errors.Is(err, ErrInvalidContainer):
			apierr.WriteError(w, http.StatusUnprocessableEntity, "invalid_container", err.Error())
		default:
			slog.Warn("Docker execution operation failed", "operation", r.PathValue("operation"), "workload_id", input.Ref.WorkloadID, "error", err)
			apierr.WriteError(w, http.StatusServiceUnavailable, "runtime_unavailable", "Docker execution operation failed")
		}
	})
	return mux
}

type Bridge struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewBridge(baseURL, token string) (*Bridge, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid Workspace Manager URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Bridge{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client: &http.Client{
			Timeout:       9 * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (b *Bridge) call(ctx context.Context, operation string, input bridgeInput, dst any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/internal/core/docker/"+operation, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode == http.StatusUnprocessableEntity {
		var detail apierr.ErrorResponse
		if err := decoder.Decode(&detail); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s", ErrInvalidContainer, detail.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Docker bridge %s returned HTTP %d", operation, resp.StatusCode)
	}
	return decoder.Decode(dst)
}

func (b *Bridge) Capacity(ctx context.Context) (DockerCapacity, error) {
	var capacity DockerCapacity
	err := b.call(ctx, "capacity", bridgeInput{}, &capacity)
	return capacity, err
}

func (b *Bridge) Create(ctx context.Context, ref ExecutionRef, spec ContainerSpec) error {
	return b.call(ctx, "create", bridgeInput{Ref: ref, Spec: spec}, &struct{}{})
}

func (b *Bridge) Start(ctx context.Context, ref ExecutionRef) error {
	return b.call(ctx, "start", bridgeInput{Ref: ref}, &struct{}{})
}

func (b *Bridge) Inspect(ctx context.Context, ref ExecutionRef) (ExecutionStatus, error) {
	var status ExecutionStatus
	err := b.call(ctx, "inspect", bridgeInput{Ref: ref}, &status)
	return status, err
}

func (b *Bridge) Stop(ctx context.Context, ref ExecutionRef) error {
	return b.call(ctx, "stop", bridgeInput{Ref: ref}, &struct{}{})
}

func (b *Bridge) Remove(ctx context.Context, ref ExecutionRef) error {
	return b.call(ctx, "remove", bridgeInput{Ref: ref}, &struct{}{})
}

func (b *Bridge) Logs(ctx context.Context, ref ExecutionRef) (string, error) {
	var logs string
	err := b.call(ctx, "logs", bridgeInput{Ref: ref}, &logs)
	return logs, err
}
