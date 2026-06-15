package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// maxRequestBodyBytes bounds how much of a request body the proxy will buffer
// before extracting the model name. Caps a memory-exhaustion DoS vector.
const maxRequestBodyBytes = 32 << 20 // 32 MiB

type Handler struct {
	router *router.Router
	admin  *admin.Server
	audit  *audit.Logger
	access *AccessLogger
	auth   *auth.Middleware
}

func NewHandler(r *router.Router, a *admin.Server, al *audit.Logger) *Handler {
	// access defaults to a no-op logger; main wires a real one via SetAccessLogger.
	return &Handler{router: r, admin: a, audit: al, access: NewAccessLogger(nil, false)}
}

// SetAuth wires the auth middleware so the proxy can refund a key's rate-limit
// and quota budget when a request is rejected by policy (model allow-list)
// before ever reaching a node. Optional; nil keeps refunds disabled.
func (h *Handler) SetAuth(m *auth.Middleware) {
	h.auth = m
}

// SetAccessLogger installs the structured access logger. Passing nil keeps the
// existing no-op logger so callers never have to nil-check.
func (h *Handler) SetAccessLogger(l *AccessLogger) {
	if l != nil {
		h.access = l
	}
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

	// Feature 3: intercept GET /v1/models and return aggregated OpenAI-schema list.
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		h.serveModels(w)
		return
	}

	var body []byte
	if r.Body != nil {
		var err error
		// Bound per-request memory: a single client cannot force the proxy to
		// buffer an unbounded body. 32 MiB is generous for prompts and
		// base64-encoded images while still capping a DoS vector.
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
		if err != nil {
			status := http.StatusBadRequest
			msg := `{"error":"read body"}`
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				status = http.StatusRequestEntityTooLarge
				msg = `{"error":"request body exceeds 32MiB limit"}`
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write([]byte(msg))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	modelName := router.ExtractModelName(body)

	// Enforce per-key model allow-list. An empty list means no restriction.
	if allowed := auth.AllowedModelsFromContext(r.Context()); len(allowed) > 0 {
		permitted := false
		for _, m := range allowed {
			if m == modelName {
				permitted = true
				break
			}
		}
		if !permitted {
			// The request is rejected by policy before reaching any node, so
			// refund the rate-limit token and quota count auth consumed - a
			// disallowed model must not burn the key's budget.
			if h.auth != nil {
				h.auth.Refund(keyName)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"error":"model %q not allowed for this api key"}`, modelName)
			metrics.RequestsTotal(keyName, modelName, "none", "403")
			return
		}
	}

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

	targetURL, err := url.Parse(node.URL)
	if err != nil {
		h.router.DecrConn(node)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"invalid node URL"}`))
		return
	}

	// Feature 2: custom transport with ResponseHeaderTimeout so a node that
	// accepts the connection but hangs (model load stall, GPU OOM) does not
	// block the client or leak goroutines. This covers only the wait for
	// response headers - NOT the streaming body - so R2 (no buffering) is safe.
	transport := &http.Transport{
		ResponseHeaderTimeout: h.router.UpstreamTimeout(),
	}

	// Feature 1: retry/failover loop. The ErrorHandler fires only when the
	// upstream failed before sending any response bytes, so retrying a
	// different node is safe and does not violate R2.
	tried := map[string]bool{node.URL: true}
	maxRetries := h.router.MaxRetries()

	var (
		rec     *statusRecorder
		aborted bool
	)

	for attempt := 0; ; attempt++ {
		proxy := buildLocalProxy(targetURL, body, r, transport)

		// retryNode is set inside the ErrorHandler closure when a retry is
		// needed. Using a pointer-to-pointer lets the closure write through to
		// a variable in this stack frame without heap allocation overhead.
		var nextNode *router.NodeState
		var retryErr error
		// errHandled is set when ErrorHandler fired and already released this
		// node's connection slot, so the post-loop DecrConn must not run again
		// (a double decrement skews least-connections toward the failed node).
		var errHandled bool

		// origReq is captured here so proxyToCloud receives the original
		// client request, not the modified upstream request that ErrorHandler
		// receives after Director has mutated it.
		origReq := r
		proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, e error) {
			// Upstream failed before writing any bytes - safe to retry.
			h.router.DecrConn(node)
			errHandled = true
			tried[node.URL] = true

			// If the client already disconnected, do not burn an alternate node
			// or a cloud call on a request nobody is waiting for.
			if origReq.Context().Err() != nil {
				retryErr = origReq.Context().Err()
				return
			}

			if attempt < maxRetries {
				alt, _ := h.router.RouteExcluding(modelName, tried)
				if alt != nil {
					metrics.Retry(node.Name)
					nextNode = alt
					retryErr = e
					return
				}
			}
			// No alternate nodes - try cloud fallback.
			cloud := h.router.RouteCloud()
			if cloud != nil {
				// Signal cloud path via sentinel before calling proxyToCloud,
				// which writes the response. The outer loop checks this after
				// serveAndRecoverAbort returns.
				retryErr = errCloudHandled
				h.proxyToCloud(rw, origReq, body, modelName, keyName, requestID, start, cloud)
				return
			}
			// Log detail server-side; return a generic message so upstream
			// topology never leaks to the client.
			log.Printf("upstream error (node=%s request_id=%s): %v", node.Name, requestID, e)
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusBadGateway)
			rw.Write([]byte(`{"error":"upstream unavailable"}`))
		}

		rec = &statusRecorder{ResponseWriter: w}
		aborted = serveAndRecoverAbort(proxy, rec, r)

		if retryErr == errCloudHandled {
			// Cloud path handled the response and did its own logging.
			return
		}

		if nextNode != nil {
			// Switch to the alternate node and retry.
			node = nextNode
			h.router.IncrConn(node)
			targetURL, err = url.Parse(node.URL)
			if err != nil {
				h.router.DecrConn(node)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":"invalid node URL"}`))
				return
			}
			warm = false // retried node may not have model warm
			continue
		}

		// Success or terminal failure - exit retry loop. Skip the decrement if
		// ErrorHandler already released this node's slot.
		if !errHandled {
			h.router.DecrConn(node)
		}
		break
	}

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
	h.access.Log(AccessLogEntry{
		TimeUnixMs: time.Now().UnixMilli(),
		RequestID:  requestID,
		KeyName:    keyName,
		Model:      modelName,
		Node:       node.Name,
		Status:     rec.statusCode,
		LatencyMs:  int64(time.Since(start).Milliseconds()),
		Cloud:      false,
	})
}

// errCloudHandled is a sentinel used inside the ErrorHandler closure to signal
// that the cloud fallback path has already written the response.
var errCloudHandled = fmt.Errorf("cloud handled")

// buildLocalProxy constructs a reverse proxy for the given node URL.
// It is extracted so the retry loop can rebuild it cleanly for each attempt.
func buildLocalProxy(targetURL *url.URL, body []byte, orig *http.Request, transport *http.Transport) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = transport
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = orig.URL.Path
		req.URL.RawQuery = orig.URL.RawQuery
		req.Host = targetURL.Host
		req.Header = make(http.Header)
		for k, v := range orig.Header {
			if k != "Authorization" {
				req.Header[k] = v
			}
		}
		if len(body) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
	}
	return proxy
}

// serveModels handles GET /v1/models by returning an OpenAI-schema list of
// models currently loaded across all healthy nodes. Only models actually
// reported by /api/ps are included - no fabricated entries.
func (h *Handler) serveModels(w http.ResponseWriter) {
	seen := make(map[string]struct{})
	for _, n := range h.router.Nodes() {
		n.RLock()
		healthy := n.Healthy
		models := n.LoadedModels
		n.RUnlock()
		if !healthy {
			continue
		}
		for _, m := range models {
			seen[m.Name] = struct{}{}
		}
	}

	// Stable sort for deterministic output.
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	type response struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}

	data := make([]modelEntry, 0, len(names))
	for _, name := range names {
		data = append(data, modelEntry{
			ID:      name,
			Object:  "model",
			OwnedBy: "ollama-mesh",
		})
	}

	out, _ := json.Marshal(response{Object: "list", Data: data})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
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
	metrics.CloudFallback(cloud.Name)
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
	// When the original client path is Ollama-native (/api/chat or
	// /api/generate) wrap the transport to translate the OpenAI response back
	// into Ollama NDJSON. For /v1/... paths the cloud response passes through
	// unchanged (current behavior preserved).
	if isOllamaPath(r.URL.Path) {
		proxy.Transport = &translatingTransport{
			inner:        http.DefaultTransport,
			origPath:     r.URL.Path,
			clientModel:  modelName,
			clientStream: clientWantsStream(body),
		}
	}
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
		// Log the detailed error server-side, but never leak upstream topology
		// (hostnames, ports, dial/TLS details) to the client.
		log.Printf("cloud upstream error (provider=%s request_id=%s): %v", cloud.Name, requestID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"upstream unavailable"}`))
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
	h.access.Log(AccessLogEntry{
		TimeUnixMs: time.Now().UnixMilli(),
		RequestID:  requestID,
		KeyName:    keyName,
		Model:      loggedModel,
		Node:       nodeName,
		Status:     rec.statusCode,
		LatencyMs:  int64(time.Since(start).Milliseconds()),
		Cloud:      true,
	})
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
