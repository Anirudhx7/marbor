package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

type Handler struct {
	router *router.Router
	admin  *admin.Server
}

func NewHandler(r *router.Router, a *admin.Server) *Handler {
	return &Handler{router: r, admin: a}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	keyName := auth.KeyNameFromContext(r.Context())

	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"read body"}`))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	modelName := router.ExtractModelName(body)
	node, warm := h.router.Route(modelName)
	if node == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"no healthy nodes available"}`))
		metrics.RequestsTotal(keyName, modelName, "none", "503")
		return
	}

	h.router.IncrConn(node)
	defer h.router.DecrConn(node)

	targetURL, err := url.Parse(node.URL)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"invalid node URL"}`))
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = r.URL.Path
		req.URL.RawQuery = r.URL.RawQuery
		req.Host = targetURL.Host
		req.Header.Del("Authorization")
		for k, v := range r.Header {
			if k != "Authorization" {
				req.Header[k] = v
			}
		}
		if len(body) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"upstream error: %s"}`, err.Error())
	}

	rec := &statusRecorder{ResponseWriter: w}
	proxy.ServeHTTP(rec, r)

	duration := time.Since(start).Seconds()
	metrics.RequestsTotal(keyName, modelName, node.Name, rec.Status())
	metrics.RequestDuration(modelName, node.Name, duration)

	// Log to admin live requests
	status := "warm"
	if !warm {
		status = "loading"
	}
	if rec.statusCode >= 500 {
		status = "error"
	}
	latencyMs := int(time.Since(start).Milliseconds())
	if rec.statusCode >= 500 {
		latencyMs = 0 // error requests show as instant fail in UI
	}
	if h.admin != nil {
		h.admin.LogRequest(keyName, modelName, node.Name, status, latencyMs)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.statusCode == 0 {
		r.statusCode = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Status() string {
	if r.statusCode == 0 {
		return "200"
	}
	return fmt.Sprintf("%d", r.statusCode)
}
