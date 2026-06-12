package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

type Handler struct {
	router *router.Router
	admin  *admin.Server
	audit  *audit.Logger
}

func NewHandler(r *router.Router, a *admin.Server, al *audit.Logger) *Handler {
	return &Handler{router: r, admin: a, audit: al}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	keyName := auth.KeyNameFromContext(r.Context())

	// Generate request ID for tracing
	var requestID string
	b := make([]byte, 8)
	if _, err := rand.Read(b); err == nil {
		requestID = hex.EncodeToString(b)
	}
	w.Header().Set("X-Request-ID", requestID)

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
		cloud := h.router.RouteCloud()
		if cloud != nil {
			h.proxyToCloud(w, r, body, modelName, keyName, requestID, start, cloud)
			return
		}
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
	aborted := serveAndRecoverAbort(proxy, rec, r)

	duration := time.Since(start).Seconds()
	metricStatus := rec.Status()
	if aborted {
		metricStatus = "aborted"
	}
	metrics.RequestsTotal(keyName, modelName, node.Name, metricStatus)
	metrics.RequestDuration(modelName, node.Name, duration)

	// Log to admin live requests
	status := "warm"
	if !warm {
		status = "loading"
	}
	if rec.statusCode >= 500 {
		status = "error"
	}
	if aborted {
		status = "aborted"
	}
	latencyMs := int(time.Since(start).Milliseconds())
	if rec.statusCode >= 500 {
		latencyMs = 0 // error requests show as instant fail in UI
	}
	if h.admin != nil {
		tokens := rec.tokenCount()
		h.admin.LogRequest(keyName, modelName, node.Name, status, latencyMs, tokens)
		h.admin.TrackLocalRequestModel(modelName, tokens)
	}
	if h.audit != nil {
		h.audit.Log(audit.Entry{
			Time:      time.Now(),
			RequestID: requestID,
			KeyName:   keyName,
			Model:     modelName,
			Node:      node.Name,
			Status:    status,
			LatencyMs: latencyMs,
			Cloud:     false,
		})
	}
}

// serveAndRecoverAbort runs the reverse proxy and absorbs http.ErrAbortHandler,
// which httputil.ReverseProxy panics with when the upstream dies mid-stream.
// Without this, the net/http server recovers the panic above us and every
// post-proxy step (metrics, admin log, audit) is silently skipped. Returns
// true when the stream was aborted. The panic is intentionally not re-raised:
// the partial body has already been written, the connection cannot be reused
// either way, and swallowing it lets the request be recorded as "aborted".
// Any other panic value is re-raised untouched.
func serveAndRecoverAbort(proxy *httputil.ReverseProxy, w http.ResponseWriter, r *http.Request) (aborted bool) {
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				aborted = true
				return
			}
			panic(p)
		}
	}()
	proxy.ServeHTTP(w, r)
	return false
}

func (h *Handler) proxyToCloud(w http.ResponseWriter, r *http.Request, body []byte, modelName, keyName, requestID string, start time.Time, cloud *config.CloudProvider) {
	path := translateCloudPath(r.URL.Path)

	outBody := body
	// loggedModel makes model rewriting visible: "<original> -> <cloud model>"
	// in the request log when the cloud provider's default_model replaced the
	// client's requested model, plain "<original>" otherwise.
	loggedModel := modelName
	cloudModel := ""
	if cloud.DefaultModel != "" && len(body) > 0 {
		outBody = rewriteModelField(body, cloud.DefaultModel)
		cloudModel = cloud.DefaultModel
		loggedModel = modelName + " -> " + cloud.DefaultModel
	}

	targetURL, err := url.Parse(cloud.BaseURL)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"invalid cloud provider URL"}`))
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = path
		req.URL.RawQuery = r.URL.RawQuery
		req.Host = targetURL.Host
		req.Header = r.Header.Clone()
		req.Header.Del("Authorization")
		req.Header.Set("Authorization", "Bearer "+cloud.APIKey)
		if len(outBody) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(outBody))
			req.ContentLength = int64(len(outBody))
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"cloud upstream error: %s"}`, err.Error())
	}

	rec := &statusRecorder{ResponseWriter: w}
	aborted := serveAndRecoverAbort(proxy, rec, r)

	duration := time.Since(start).Seconds()
	nodeName := "cloud:" + cloud.Name
	status := "cloud"
	metricStatus := rec.Status()
	if aborted {
		status = "aborted"
		metricStatus = "aborted"
	}
	metrics.RequestsTotal(keyName, modelName, nodeName, metricStatus)
	metrics.RequestDuration(modelName, nodeName, duration)

	if h.admin != nil {
		latencyMs := int(time.Since(start).Milliseconds())
		tokens := rec.tokenCount()
		h.admin.LogRequest(keyName, loggedModel, nodeName, status, latencyMs, tokens)
		h.admin.TrackCloudCostModel(modelName, cloud.CostPer1KTokens, tokens)
	}
	if h.audit != nil {
		h.audit.Log(audit.Entry{
			Time:       time.Now(),
			RequestID:  requestID,
			KeyName:    keyName,
			Model:      modelName,
			Node:       nodeName,
			Status:     status,
			LatencyMs:  int(time.Since(start).Milliseconds()),
			Cloud:      true,
			CloudModel: cloudModel,
		})
	}
}

func translateCloudPath(ollamaPath string) string {
	switch ollamaPath {
	case "/api/chat":
		return "/v1/chat/completions"
	case "/api/generate":
		return "/v1/completions"
	case "/api/embeddings":
		return "/v1/embeddings"
	default:
		return ollamaPath
	}
}

func rewriteModelField(body []byte, model string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	b, err := json.Marshal(model)
	if err != nil {
		return body
	}
	m["model"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
	tail       []byte // last tailMax bytes written, for token-count parsing
}

// tailMax bounds the retained response tail. Token counts live in the final
// JSON object (Ollama NDJSON) or final SSE chunk (OpenAI), so a small tail
// is enough. Writes still pass straight through — streaming is not buffered.
const tailMax = 8192

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	if n > 0 {
		r.tail = append(r.tail, b[:n]...)
		if len(r.tail) > tailMax {
			r.tail = r.tail[len(r.tail)-tailMax:]
		}
	}
	return n, err
}

// tokenCount parses real token usage from the response tail. It scans lines
// from the end looking for Ollama's final object (eval_count +
// prompt_eval_count) or an OpenAI-style usage block (total_tokens).
// Returns 0 when no count is present — callers treat 0 as "unknown".
func (r *statusRecorder) tokenCount() int64 {
	lines := bytes.Split(r.tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		line = bytes.TrimPrefix(line, []byte("data: ")) // SSE framing
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var t struct {
			EvalCount       int64 `json:"eval_count"`
			PromptEvalCount int64 `json:"prompt_eval_count"`
			Usage           struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(line, &t); err != nil {
			continue
		}
		if n := t.EvalCount + t.PromptEvalCount; n > 0 {
			return n
		}
		if t.Usage.TotalTokens > 0 {
			return t.Usage.TotalTokens
		}
	}
	return 0
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
