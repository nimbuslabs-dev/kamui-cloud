package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
)

type RegistrationResponse struct {
	ID                 string `json:"id"`
	HeartbeatIntervalS int    `json:"heartbeat_interval_s"`
	OfflineAfterS      int    `json:"offline_after_s"`
}

type NodeHandler struct {
	reg *registry.Registry
}

func NewNodeHandler(reg *registry.Registry) *NodeHandler {
	return &NodeHandler{reg: reg}
}

func (h *NodeHandler) Register(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	var info registry.NodeInfo
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&info); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}

	if dec.More() {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "extra data")
		return
	}

	if info.Hostname == "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "hostname is required")
		return
	}

	switch info.OS {
	case "macos", "windows", "linux":
	default:
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid os")
		return
	}

	switch info.Arch {
	case "arm64", "amd64":
	default:
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid arch")
		return
	}

	if info.CPUCores < 1 {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "cpu_cores must be >= 1")
		return
	}

	if info.RAMMB < 1 {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "ram_mb must be >= 1")
		return
	}

	if info.IP == "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "ip is required")
		return
	}

	for _, role := range info.Roles {
		switch role {
		case "store", "run", "train", "infer", "drop", "console":
		default:
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid role")
			return
		}
	}

	id := h.reg.Register(info)

	apierr.WriteJSON(w, http.StatusCreated, RegistrationResponse{
		ID:                 id,
		HeartbeatIntervalS: 5,
		OfflineAfterS:      15,
	})
}

func (h *NodeHandler) List(w http.ResponseWriter, r *http.Request) {
	nodes := h.reg.List()
	if nodes == nil {
		nodes = []registry.Node{}
	}
	apierr.WriteJSON(w, http.StatusOK, nodes)
}

func (h *NodeHandler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "node not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	var load *registry.Load
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > 0 {
		var l registry.Load
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
			return
		}
		if dec.More() {
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "extra data")
			return
		}
		if l.CPUPct != nil && (*l.CPUPct < 0 || *l.CPUPct > 100) {
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid cpu_pct")
			return
		}
		if l.MemPct != nil && (*l.MemPct < 0 || *l.MemPct > 100) {
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid mem_pct")
			return
		}
		load = &l
	}

	if err := h.reg.Heartbeat(id, load); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "node not found")
			return
		}
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *NodeHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "node not found")
		return
	}

	if err := h.reg.Remove(id); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "node not found")
			return
		}
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
