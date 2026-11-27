package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

type ctxKey struct{}

type TokenValidator interface {
	AuthenticateToken(ctx context.Context, rawToken string) (identity.Principal, error)
}

func WithPrincipal(ctx context.Context, p identity.Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) (identity.Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(identity.Principal)
	return p, ok
}

func OperatorPrincipal() identity.Principal {
	return identity.Principal{
		User: identity.User{
			ID:            identity.LocalAdminID,
			Username:      "admin",
			DisplayName:   "Local Administrator",
			IsSystemAdmin: true,
		},
		AuthMethod:      "operator_token",
		ActiveProjectID: identity.DefaultProject,
	}
}

func Middleware(token string) func(http.Handler) http.Handler {
	return SessionMiddleware(token, nil)
}

func SessionMiddleware(token string, validator TokenValidator) func(http.Handler) http.Handler {
	expectedHash := sha256.Sum256([]byte(token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok, err := authenticateRequest(r, expectedHash, validator)
			if err != nil {
				apierr.WriteError(w, http.StatusServiceUnavailable, "auth_unavailable", "authentication store unavailable")
				return
			}
			if !ok {
				apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func OptionalSessionMiddleware(token string, validator TokenValidator) func(http.Handler) http.Handler {
	expectedHash := sha256.Sum256([]byte(token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok, err := authenticateRequest(r, expectedHash, validator); err == nil && ok {
				r = r.WithContext(WithPrincipal(r.Context(), p))
			}
			next.ServeHTTP(w, r)
		})
	}
}

func authenticateRequest(r *http.Request, expectedHash [32]byte, validator TokenValidator) (identity.Principal, bool, error) {
	authHeader := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return identity.Principal{}, false, nil
	}
	provided := authHeader[len(prefix):]
	if provided == "" {
		return identity.Principal{}, false, nil
	}
	providedHash := sha256.Sum256([]byte(provided))
	if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1 {
		return OperatorPrincipal(), true, nil
	}
	if validator == nil {
		return identity.Principal{}, false, nil
	}
	p, err := validator.AuthenticateToken(r.Context(), provided)
	if err != nil {
		if err == identity.ErrUnauthorized {
			return identity.Principal{}, false, nil
		}
		return identity.Principal{}, false, err
	}
	return p, true, nil
}
