package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func catchPanic(f func()) (p any) {
	defer func() { p = recover() }()
	f()
	return nil
}

func TestRecoverMiddlewareFlushCountsAsWrittenHeader(t *testing.T) {
	h := RecoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		panic("after flush")
	}))
	rec := httptest.NewRecorder()
	p := catchPanic(func() { h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/x", nil)) })
	if p != http.ErrAbortHandler {
		t.Errorf("panic after Flush: recovered %v, want http.ErrAbortHandler re-raised", p)
	}
}

func TestRecoverMiddlewarePanicAfterBytesAbortsConnection(t *testing.T) {
	h := RecoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("partial"))
		panic("mid body")
	}))
	rec := httptest.NewRecorder()
	p := catchPanic(func() { h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/x", nil)) })
	if p != http.ErrAbortHandler {
		t.Errorf("panic after body bytes: recovered %v, want http.ErrAbortHandler re-raised", p)
	}
}

func TestRecoverMiddlewareProxyPathUsesOpenAIEnvelope(t *testing.T) {
	h := RecoverMiddleware(panicHandler("boom"))
	for _, path := range []string{"/v1/chat/completions", "/api/generate"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		var env apiError
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Message == "" || env.Error.Type == "" {
			t.Errorf("%s: want OpenAI error envelope, got %q", path, rec.Body.String())
		}
		if rec.Code != 500 {
			t.Errorf("%s: status %d, want 500", path, rec.Code)
		}
	}
}

func TestRecoverMiddlewareLogEscapesPath(t *testing.T) {
	buf := captureLog(t)
	h := RecoverMiddleware(panicHandler("boom"))
	req := httptest.NewRequest("GET", "/x", nil)
	req.URL.Path = "/x\nFORGED"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(buf.String(), `path="/x\nFORGED"`) {
		t.Errorf("panic log does not quote the path: %q", buf.String())
	}
}
