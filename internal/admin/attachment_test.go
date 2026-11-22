package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAttachmentSetsDownloadHeader(t *testing.T) {
	h := attachment("marbor.json", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grafana/marbor.json", nil))
	if got, want := rec.Header().Get("Content-Disposition"), `attachment; filename="marbor.json"`; got != want {
		t.Fatalf("Content-Disposition = %q, want %q", got, want)
	}
	if rec.Body.String() != "{}" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
