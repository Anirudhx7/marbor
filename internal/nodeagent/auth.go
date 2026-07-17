package nodeagent

import (
	"net/http"
	"strings"
)

// checkToken reports whether authHeader (the raw Authorization header value)
// presents expectedToken as a bearer credential. Mirrors R4's discipline
// (admin.go's exact-match Bearer check): strings.TrimPrefix + ==, never
// strings.Contains. An empty expectedToken NEVER authenticates any request,
// including one with an empty bearer value ("Authorization: Bearer ") - an
// agent started without a configured token must reject everything, not fall
// open (same "never substitute/accept empty-string secret" lesson R8
// documents for the mesh side, applied here to the agent side of the same
// trust boundary).
func checkToken(authHeader, expectedToken string) bool {
	if expectedToken == "" {
		return false
	}
	return strings.TrimPrefix(authHeader, "Bearer ") == expectedToken
}

// requireToken wraps h with a bearer-token check against expectedToken,
// returning 401 on any mismatch (including an unset expectedToken, which
// rejects every request rather than accepting none).
func requireToken(expectedToken string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkToken(r.Header.Get("Authorization"), expectedToken) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		h(w, r)
	}
}
