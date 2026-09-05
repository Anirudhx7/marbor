package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"

	"github.com/Anirudhx7/marbor/internal/metrics"
)

// recoverWriter wraps http.ResponseWriter to track whether the response has
// started (WriteHeader or Write called). It delegates Flush to the underlying
// writer when it implements http.Flusher, preserving streaming.
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
// This keeps SSE / NDJSON streaming intact.
func (rw *recoverWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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
//  3. Writes HTTP 500 JSON - but only when no response bytes have been sent yet.
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

			log.Printf("PANIC recovered: method=%s path=%s request_id=%s panic=%v\n%s",
				r.Method,
				r.URL.Path,
				requestID,
				p,
				debug.Stack(),
			)

			if !rw.wroteHeader {
				body, _ := json.Marshal(map[string]string{"error": "internal server error"})
				rw.ResponseWriter.Header().Set("Content-Type", "application/json")
				rw.ResponseWriter.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(rw.ResponseWriter, "%s", body)
			}
		}()

		next.ServeHTTP(rw, r)
	})
}
