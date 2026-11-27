package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/connect"
)

type ConnectHandler struct {
	store   *connect.Store
	cliPort int
	scheme  string
}

func newConnectHandler(store *connect.Store, cliPort int, scheme string) *ConnectHandler {
	if cliPort <= 0 {
		cliPort = 8443
	}
	if scheme == "" {
		scheme = "https"
	}
	return &ConnectHandler{store: store, cliPort: cliPort, scheme: scheme}
}

func writeConnectError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, connect.ErrForbidden):
		apierr.WriteError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, connect.ErrNotFound):
		apierr.WriteError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, connect.ErrPairingExpired):
		apierr.WriteError(w, http.StatusGone, "pairing_expired", err.Error())
	case errors.Is(err, connect.ErrPairingUsed):
		apierr.WriteError(w, http.StatusConflict, "pairing_used", err.Error())
	case errors.Is(err, connect.ErrPairingRevoked):
		apierr.WriteError(w, http.StatusForbidden, "pairing_revoked", err.Error())
	case errors.Is(err, connect.ErrBadInput):
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "connectivity operation failed")
	}
}

func (h *ConnectHandler) Info(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePrincipal(w, r); !ok {
		return
	}
	info, err := h.store.Info(r.Context(), h.cliPort, h.scheme)
	if err != nil {
		writeConnectError(w, err)
		return
	}
	if r.URL.Query().Get("mdns") == "1" {
		if svcs, mErr := connect.DiscoverMDNS(r.Context(), 800*time.Millisecond); mErr == nil {
			for _, svc := range svcs {
				info.Candidates = append(info.Candidates, connect.Endpoint{
					URL:      "https://" + svc.Address + ":" + strconvItoa(svc.Port),
					Kind:     "mdns",
					Address:  svc.Address,
					Port:     svc.Port,
					Hostname: svc.Host,
					TLS:      true,
				})
			}
		}
	}
	apierr.WriteJSON(w, http.StatusOK, info)
}

func strconvItoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func (h *ConnectHandler) CreatePairing(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	var in connect.CreatePairingInput
	if r.ContentLength > 0 {
		if err := readJSON(w, r, 8*1024, &in); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid pairing request")
			return
		}
	}
	challenge, err := h.store.CreatePairing(r.Context(), p, in)
	if err != nil {
		writeConnectError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, challenge)
}

func (h *ConnectHandler) RevokePairing(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.store.RevokePairing(r.Context(), p.User.ID, p.User.IsSystemAdmin, r.PathValue("id")); err != nil {
		writeConnectError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ConnectHandler) RedeemPairing(w http.ResponseWriter, r *http.Request) {
	var in connect.RedeemPairingInput
	if err := readJSON(w, r, 8*1024, &in); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid pairing redeem request")
		return
	}
	res, err := h.store.RedeemPairing(r.Context(), in)
	if err != nil {
		writeConnectError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, res)
}

func (h *ConnectHandler) ListDevices(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	devices, err := h.store.ListDevices(r.Context(), p.User.ID, p.User.IsSystemAdmin)
	if err != nil {
		writeConnectError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, devices)
}

func (h *ConnectHandler) ApproveDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	dev, err := h.store.ApproveDevice(r.Context(), p.User.ID, p.User.IsSystemAdmin, r.PathValue("id"))
	if err != nil {
		writeConnectError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, dev)
}

func (h *ConnectHandler) RevokeDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.store.RevokeDevice(r.Context(), p.User.ID, p.User.IsSystemAdmin, r.PathValue("id")); err != nil {
		writeConnectError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ConnectHandler) GetRemoteConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePrincipal(w, r); !ok {
		return
	}
	cfg, err := h.store.GetRemoteConfig(r.Context())
	if err != nil {
		writeConnectError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, cfg)
}

func (h *ConnectHandler) UpdateRemoteConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := requireSystemAdmin(w, r)
	if !ok {
		return
	}
	var cfg connect.RemoteConfig
	if err := readJSON(w, r, 8*1024, &cfg); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid remote connectivity payload")
		return
	}
	updated, err := h.store.UpdateRemoteConfig(r.Context(), p.User.ID, cfg)
	if err != nil {
		writeConnectError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, updated)
}

func (h *ConnectHandler) DisableRemoteConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := requireSystemAdmin(w, r)
	if !ok {
		return
	}
	updated, err := h.store.UpdateRemoteConfig(r.Context(), p.User.ID, connect.RemoteConfig{Enabled: false, Provider: "none"})
	if err != nil {
		writeConnectError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, updated)
}

func requireAdminOrPrincipal(r *http.Request) (string, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		return "", false
	}
	return p.User.ID, p.User.IsSystemAdmin
}
