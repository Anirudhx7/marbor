package proxy

import "net/http"

// SecurityHeaders sets baseline security response headers on every request
// (2026-07-14 audit, Priority 3). Applied to both the proxy and admin HTTP
// servers in main.go, since they're two independent http.Server instances
// with no other shared middleware point besides RecoverMiddleware.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"style-src 'self' https://fonts.googleapis.com; "+
				"font-src 'self' https://fonts.gstatic.com; "+
				"img-src 'self' data:; "+
				"script-src 'self'; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
