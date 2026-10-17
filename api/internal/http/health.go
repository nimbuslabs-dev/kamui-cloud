package httpapi

import (
	"net/http"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
)

type HealthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

func HandleHealthz(w http.ResponseWriter, r *http.Request) {
	apierr.WriteJSON(w, http.StatusOK, HealthResponse{
		Status:  "ok",
		Service: "api",
		Version: "0.1.0",
	})
}
