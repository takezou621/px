package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireBearer wraps h so every request must carry
// "Authorization: Bearer <token>". /healthz stays open so probes can tell
// the server is up without holding the token. Comparison is constant-time;
// the token must be non-empty (an empty token would match any
// "Bearer " prefix and disable auth entirely).
func RequireBearer(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="px"`)
			httpError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.ServeHTTP(w, r)
	})
}
