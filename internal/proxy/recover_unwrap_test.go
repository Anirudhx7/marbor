package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A handler behind RecoverMiddleware must be able to set per-request
// deadlines; without Unwrap the ResponseController gets ErrNotSupported and
// long transfers die at the server-wide timeouts.
func TestRecoverMiddlewareLetsResponseControllerReachTheConnection(t *testing.T) {
	var deadlineErr error
	h := RecoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		deadlineErr = rc.SetWriteDeadline(time.Now().Add(time.Minute))
		w.WriteHeader(http.StatusNoContent)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if deadlineErr != nil {
		t.Fatalf("SetWriteDeadline through RecoverMiddleware = %v, want nil", deadlineErr)
	}
}
