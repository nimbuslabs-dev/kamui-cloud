package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
)

func Middleware(token string) func(http.Handler) http.Handler {
	expectedHash := sha256.Sum256([]byte(token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(authHeader, prefix) {
				apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
				return
			}
			provided := authHeader[len(prefix):]
			providedHash := sha256.Sum256([]byte(provided))
			if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
				apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
