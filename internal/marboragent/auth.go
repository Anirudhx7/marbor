package marboragent

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

// tier is a Marbor Agent token's authorization level (P54). Tiers are ordinal:
// a token authorizes its own tier and every tier below it. tierAdmin is
// deliberately the ceiling for every route today - no current route
// requires it, it exists only so a future Group 3 ("Maintain") action can
// require it and be correctly refused by every token that predates that
// action, per .local/specs/node-agent-capabilities.md section 7.
type tier int

const (
	// tierReadonly authorizes Group 1 "Observe" routes only (status, metrics).
	tierReadonly tier = iota
	// tierOperator authorizes Group 1 + Group 2 "Operate" routes (models
	// pull/list/delete/unload, runtime start/stop/restart/logs/disk, health
	// check) - today's full route surface.
	tierOperator
	// tierAdmin authorizes everything, including any future Group 3
	// "Maintain" route. Reserved: no route requires this tier yet.
	tierAdmin
)

// Exported scope names - the wire contract for token generation
// (admin.go's generateMarborAgentToken) and parsing (scopeOf below). A token
// is "<scope>." + a random secret; these are the only three recognized
// prefixes. Keep these names in sync with tierNames.
const (
	ScopeReadonly = "readonly"
	ScopeOperator = "operator"
	ScopeAdmin    = "admin"
)

// tierNames maps the token's embedded scope prefix (see scopeOf) to its
// tier. Order/spelling here is the wire contract for token generation
// (admin.go's generateMarborAgentToken) and parsing (scopeOf) - keep in sync.
var tierNames = map[string]tier{
	ScopeReadonly: tierReadonly,
	ScopeOperator: tierOperator,
	ScopeAdmin:    tierAdmin,
}

// scopeOf parses the tier embedded in token's prefix (P54 design: "<tier>."
// + random secret, e.g. "operator.Xk9f..."). "." never appears in the
// base64url alphabet the random suffix is drawn from, so splitting on the
// first "." is unambiguous.
//
// A token with NO "." at all - every token minted before this feature
// existed, a bare random string with no prefix - parses as tierAdmin. This
// is the deliberate backward-compat path: upgrading the agent binary to one
// that enforces scope must not lock an already-enrolled node out of its own
// existing full-access token merely because that token predates the scoping
// feature.
//
// A token that DOES contain a "." but whose left segment isn't one of
// tierNames is a different case and must NOT share that fallback: a real
// legacy token can never contain "." (it's outside the base64url alphabet),
// so anything reaching this branch is either corrupted or a name this
// version doesn't recognize (e.g. a scope added elsewhere without updating
// tierNames here) - failing open to tierAdmin there would hand out the
// highest privilege tier for exactly the input that's least trustworthy.
// This branch fails closed to tierReadonly instead.
func scopeOf(token string) tier {
	prefix, _, found := strings.Cut(token, ".")
	if !found {
		return tierAdmin
	}
	t, ok := tierNames[prefix]
	if !ok {
		return tierReadonly
	}
	return t
}

// TokenScope returns the scope name (ScopeReadonly/ScopeOperator/ScopeAdmin)
// embedded in token, for admin.go's token issuance and callers that need to
// report/verify what a generated token's tier actually is without
// duplicating scopeOf's parsing.
func TokenScope(token string) string {
	switch scopeOf(token) {
	case tierReadonly:
		return ScopeReadonly
	case tierOperator:
		return ScopeOperator
	default:
		return ScopeAdmin
	}
}

// requireToken wraps h with a bearer-token check against expectedToken,
// returning 401 on any mismatch (including an unset expectedToken, which
// rejects every request rather than accepting none). Equivalent to
// requireScope with tierReadonly - kept as the lowest-friction entry point
// for routes that need no scope enforcement beyond "is this a valid token".
func requireToken(expectedToken string, h http.HandlerFunc) http.HandlerFunc {
	return requireScope(expectedToken, tierReadonly, h)
}

// requireScope wraps h with a bearer-token check against expectedToken
// (401 on mismatch, identical to requireToken) plus a scope check: the
// token's own embedded tier (scopeOf(expectedToken)) must meet or exceed
// required, or the request is rejected with 403 (not 401 - the token is
// valid, it simply isn't authorized for this action, and a caller should be
// able to tell "wrong token" apart from "right token, insufficient scope").
func requireScope(expectedToken string, required tier, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkToken(r.Header.Get("Authorization"), expectedToken) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		if scopeOf(expectedToken) < required {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"insufficient token scope"}`))
			return
		}
		h(w, r)
	}
}
