package httpapi

import (
	"net/http"
)

type HealthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

func HandleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteError(w, http.StatusBadRequest, "bad_request", "method not allowed")
		return
	}
	WriteJSON(w, http.StatusOK, HealthResponse{
		Status:  "ok",
		Service: "api",
		Version: "0.1.0",
	})
}
