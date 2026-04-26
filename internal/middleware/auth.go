package middleware

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/nevup/trade-journal/internal/auth"
	"github.com/nevup/trade-journal/internal/httpx"
)

// Authenticator extracts the Bearer JWT, verifies it, and stores the userId
// in the request context so downstream handlers + middleware can rely on it.
//
//   - Missing header / malformed token / bad signature / expired → 401
//   - Valid token → context populated, request continues
//
// The 403 cross-tenant check is enforced separately in RequireUserMatch
// because not every authed endpoint has a userId path parameter (e.g. /trades).
func Authenticator(verifier *auth.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if h == "" {
				httpx.WriteError(w, r, http.StatusUnauthorized,
					"UNAUTHORIZED", "Missing Authorization header.")
				return
			}
			const prefix = "Bearer "
			if !strings.HasPrefix(h, prefix) {
				httpx.WriteError(w, r, http.StatusUnauthorized,
					"UNAUTHORIZED", "Authorization header must use Bearer scheme.")
				return
			}
			tok := strings.TrimSpace(h[len(prefix):])
			claims, err := verifier.Parse(tok)
			if err != nil {
				httpx.WriteError(w, r, http.StatusUnauthorized,
					"UNAUTHORIZED", "Invalid or expired token.")
				return
			}

			// Attach userId to BOTH the context (for handler use) and the
			// per-request logger (so subsequent log lines carry it).
			ctx := httpx.WithUserID(r.Context(), claims.UserID)
			reqLog := httpx.Logger(ctx).With("userId", claims.UserID.String())
			ctx = httpx.WithLogger(ctx, reqLog)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireUserMatch enforces the row-level tenancy rule from the spec:
//
//	"The sub claim (userId) in the JWT must exactly match the userId in
//	 the data being accessed. Any mismatch must return HTTP 403 — never 404."
//
// It reads {userId} from the URL path (chi.URLParam — but we receive it via
// the second arg to keep this package framework-agnostic).
func RequireUserMatch(pathUserID func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenUID, ok := httpx.UserID(r.Context())
			if !ok {
				// Should be impossible if Authenticator ran first, but guard.
				httpx.WriteError(w, r, http.StatusUnauthorized,
					"UNAUTHORIZED", "Missing authenticated user.")
				return
			}
			pathStr := pathUserID(r)
			pathUID, err := uuid.Parse(pathStr)
			if err != nil {
				// Malformed path UUID is a 400, not 403 — the request is
				// nonsensical regardless of who's asking.
				httpx.WriteError(w, r, http.StatusBadRequest,
					"BAD_REQUEST", "userId path parameter must be a UUID.")
				return
			}
			if tokenUID != pathUID {
				httpx.WriteError(w, r, http.StatusForbidden,
					"FORBIDDEN", "Cross-tenant access denied.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
