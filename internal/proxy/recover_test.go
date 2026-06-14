package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// panicHandler returns a handler that panics with the given value.
func panicHandler(v any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(v)
	})
}

func TestRecoverMiddlewareReturns500(t *testing.T) {
	h := RecoverMiddleware(panicHandler("something went wrong"))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "internal server error") {
		t.Fatalf("want body to contain 'internal server error', got: %s", body)
	}
}

func TestRecoverMiddlewareReabortsAbortHandler(t *testing.T) {
	h := RecoverMiddleware(panicHandler(http.ErrAbortHandler))

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rec := httptest.NewRecorder()

	var caught any
	func() {
		defer func() {
			caught = recover()
		}()
		h.ServeHTTP(rec, req)
	}()

	if caught != http.ErrAbortHandler {
		t.Fatalf("want re-panic with http.ErrAbortHandler, got: %v", caught)
	}
}

func TestRecoverMiddlewarePassesThroughOK(t *testing.T) {
	const wantBody = "hello world"

	h := RecoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, wantBody)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != wantBody {
		t.Fatalf("want body %q, got %q", wantBody, got)
	}
}

func TestRecoverFlusherPreserved(t *testing.T) {
	flushed := false

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("wrapped writer does not implement http.Flusher")
			return
		}
		f.Flush()
		flushed = true
	})

	h := RecoverMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/flush", nil)
	// httptest.NewRecorder implements http.Flusher - delegation must reach it.
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if !flushed {
		t.Fatal("Flush() was never called on the underlying writer")
	}
}
