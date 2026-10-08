package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/Anirudhx7/marbor/internal/metrics"
)

// recoverWriter wraps http.ResponseWriter to track whether the response has
// started (WriteHeader, Write or Flush called). It delegates Flush to the
// underlying writer when it implements http.Flusher, preserving streaming.
type recoverWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (rw *recoverWriter) WriteHeader(code int) {
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recoverWriter) Write(b []byte) (int, error) {
	rw.wroteHeader = true
	return rw.ResponseWriter.Write(b)
}

// Flush delegates to the underlying writer when it satisfies http.Flusher.
// This keeps SSE / NDJSON streaming intact. A flush commits the status line
// and headers (an implicit 200 if none was written), so it counts as the
// response having started.
func (rw *recoverWriter) Flush() {
	rw.wroteHeader = true
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// isProxyAPIPath reports whether p belongs to the inference proxy (OpenAI or
// Ollama-native API). This middleware also wraps the admin and metrics
// listeners, which keep their own flat error shape; only proxy paths get the
// OpenAI error envelope SDK clients expect.
func isProxyAPIPath(p string) bool {
	return strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/api/")
}

// RecoverMiddleware catches unexpected panics from downstream handlers.
//
// CRITICAL re-panic rule: if the recovered value is http.ErrAbortHandler the
// panic is re-raised immediately so net/http's streaming-abort machinery can
// handle it. Swallowing it would break mid-stream delivery.
//
// For every other non-nil panic value the middleware:
//  1. Calls metrics.Panic() to increment the counter.
//  2. Logs method, path, X-Request-ID, panic value, and stack trace.
//  3. Writes HTTP 500 JSON when no response bytes have been sent yet.
//  4. Otherwise (the response already started) re-raises http.ErrAbortHandler
//     so the client sees an aborted connection rather than a body that
//     silently stops and looks complete.
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recoverWriter{ResponseWriter: w}

		defer func() {
			p := recover()
			if p == nil {
				return
			}

			// Re-panic for abort handler - do not swallow.
			if p == http.ErrAbortHandler {
				panic(p)
			}

			metrics.Panic()

			requestID := w.Header().Get("X-Request-ID")
			if requestID == "" {
				requestID = r.Header.Get("X-Request-ID")
			}

			log.Printf("PANIC recovered: method=%s path=%q request_id=%s panic=%v\n%s",
				r.Method,
				r.URL.Path,
				requestID,
				p,
				debug.Stack(),
			)

			if rw.wroteHeader {
				panic(http.ErrAbortHandler)
			}
			rw.ResponseWriter.Header().Set("Content-Type", "application/json")
			rw.ResponseWriter.WriteHeader(http.StatusInternalServerError)
			if isProxyAPIPath(r.URL.Path) {
				json.NewEncoder(rw.ResponseWriter).Encode(apiError{Error: apiErrorBody{
					Message: "internal server error",
					Type:    "server_error",
					Code:    "internal_error",
				}})
				return
			}
			body, _ := json.Marshal(map[string]string{"error": "internal server error"})
			fmt.Fprintf(rw.ResponseWriter, "%s", body)
		}()

		next.ServeHTTP(rw, r)
	})
}
