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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/audit"
	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/metrics"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// maxRequestBodyBytes bounds how much of a request body the proxy will buffer
// before extracting the model name. Caps a memory-exhaustion DoS vector.
const maxRequestBodyBytes = 32 << 20 // 32 MiB

// apiError is the OpenAI-compatible error envelope. Every non-2xx response from
// the proxy and auth middleware uses this shape so SDK clients can parse errors
// without a separate error-detection path.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// writeAPIError writes an OpenAI-schema error response. It sets Content-Type,
// calls WriteHeader, then encodes the JSON body. Callers must not write to w
// after this returns.
func writeAPIError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(apiError{Error: apiErrorBody{
		Message: message,
		Type:    errType,
		Code:    code,
	}})
}

// localOnlyBlocked checks whether keyName is configured local_only and, if
// so, fails the request closed instead of letting the caller proceed to
// CloudChain()/proxyToCloud - a local_only key's traffic must never leave
// local nodes, even when a cloud provider is configured and reachable.
// Callers must check this BEFORE computing CloudChain() at every
// cloud-fallback decision point, and must not call proxyToCloud if this
// returns true. Returns false (does nothing) for a key that is not
// local_only, so both existing call sites fall through to today's behavior
// unchanged.
func (h *Handler) localOnlyBlocked(w http.ResponseWriter, keyName, modelName string) bool {
	if h.auth == nil || !h.auth.IsLocalOnly(keyName) {
		return false
	}
	writeAPIError(w, http.StatusServiceUnavailable,
		"key is configured local_only and no local node is available; request was not sent to any cloud provider",
		"server_error", "local_only_blocked")
	metrics.RequestsTotal(keyName, modelName, "none", "503")
	if h.admin != nil {
		h.admin.IncrSpill(keyName, "blocked")
	}
	return true
}

type Handler struct {
	router *router.Router
	admin  *admin.Server
	audit  *audit.Logger
	access *AccessLogger
	auth   *auth.Middleware
	// allowManagement bypasses the default-deny management-endpoint guard when
	// true (single-tenant homelab escape hatch). Default false: destructive
	// Ollama management paths (/api/delete, /api/pull, ...) are blocked so an
	// inference-only key cannot mutate models on backend GPU nodes.
	allowManagement bool

	// cloudTransportOnce guards lazy construction of cloudTransport, a single
	// shared *http.Transport for the cloud-fallback path. It clones
	// http.DefaultTransport (keeping its connection-pool defaults) and sets
	// ResponseHeaderTimeout from routing.upstream_timeout_ms so a hung cloud
	// provider cannot leak goroutines/connections. ResponseHeaderTimeout bounds
	// only the wait for response headers, never the streaming body, so R2 holds.
	cloudTransportOnce sync.Once
	cloudTransport     *http.Transport

	// localTransportOnce guards lazy construction of localTransport, a single
	// shared *http.Transport for local node proxying. Mirrors cloudTransport:
	// one instance per Handler keeps a live idle-connection pool keyed by
	// scheme+host+port instead of allocating a fresh, poolless transport (and
	// therefore a fresh TCP/TLS handshake) on every request.
	localTransportOnce sync.Once
	localTransport     *http.Transport

	// modelLimiter enforces optional per-model rpm/tpm caps from a model's
	// configured profile (store.ModelConfig). In-process only, matching the
	// rest of this file's rate-limiting state.
	modelLimiter *modelRateLimiter

	// trustProxyHeaders gates whether X-Forwarded-For/X-Real-IP are trusted for
	// the admin request log's client IP. Default false: these headers are
	// client-supplied and forgeable by anyone who can reach the proxy directly.
	trustProxyHeaders bool
}

// cloudRoundTripper returns the shared cloud transport, constructing it once.
// ResponseHeaderTimeout is derived from the router's upstream timeout; no
// overall client Timeout is set (that would kill long streaming responses).
func (h *Handler) cloudRoundTripper() *http.Transport {
	h.cloudTransportOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = h.router.UpstreamTimeout()
		h.cloudTransport = t
	})
	return h.cloudTransport
}

// localRoundTripper returns the shared local-node transport, constructing it
// once. ResponseHeaderTimeout bounds only the wait for response headers
// (never the streaming body), so R2 holds.
func (h *Handler) localRoundTripper() *http.Transport {
	h.localTransportOnce.Do(func() {
		h.localTransport = &http.Transport{
			ResponseHeaderTimeout: h.router.UpstreamTimeout(),
		}
	})
	return h.localTransport
}

func NewHandler(r *router.Router, a *admin.Server, al *audit.Logger) *Handler {
	// access defaults to a no-op logger; main wires a real one via SetAccessLogger.
	return &Handler{router: r, admin: a, audit: al, access: NewAccessLogger(nil, false), modelLimiter: newModelRateLimiter()}
}

// blockedManagementPaths is the default-deny set of Ollama model-management /
// mutation endpoints. They must not be reachable through the multi-tenant
// proxy: any authenticated key could otherwise delete models, fill disk via
// pull, or push/create/copy on a shared backend node. Read-only inventory
// (/api/tags, /v1/models) and inference (/api/generate, /api/chat, ...) are
// intentionally absent and therefore always allowed.
var blockedManagementPaths = map[string]struct{}{
	"/api/delete": {},
	"/api/pull":   {},
	"/api/push":   {},
	"/api/create": {},
	"/api/copy":   {},
	"/api/blobs":  {},
}

// isBlockedManagementPath reports whether path is a destructive Ollama
// management endpoint. Exact match for the listed paths plus a prefix match
// for "/api/blobs/" (blob upload/check by digest).
func isBlockedManagementPath(path string) bool {
	if _, ok := blockedManagementPaths[path]; ok {
		return true
	}
	return strings.HasPrefix(path, "/api/blobs/")
}

// SetTrustProxyHeaders toggles whether X-Forwarded-For/X-Real-IP are trusted
// for the admin request log's client IP. Pass true only when the mesh sits
// behind a trusted reverse proxy/load balancer that sets these headers itself
// and is the sole path to the proxy port; otherwise a direct client can forge
// them. Default false logs r.RemoteAddr (the real TCP peer) instead.
func (h *Handler) SetTrustProxyHeaders(trust bool) {
	h.trustProxyHeaders = trust
}

// SetAllowManagementEndpoints toggles the management-endpoint guard. Pass true
// only for single-tenant deployments where the caller is trusted to manage
// models on backend nodes. Default (false) blocks them.
func (h *Handler) SetAllowManagementEndpoints(allow bool) {
	h.allowManagement = allow
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

	// Generate request ID for tracing. Falls back to nanosecond timestamp so
	// the header is never empty even if the CSPRNG fails.
	var requestID string
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		requestID = fmt.Sprintf("r%d", time.Now().UnixNano())
	} else {
		requestID = hex.EncodeToString(b)
	}
	w.Header().Set("X-Request-ID", requestID)

	// Default-deny management-endpoint guard. Destructive Ollama management
	// paths (/api/delete, /api/pull, /api/push, /api/create, /api/copy,
	// /api/blobs[/...]) are blocked before any routing/forwarding so an
	// authenticated inference key cannot mutate models on shared backend nodes.
	// Overridable per-deploy via routing.allow_management_endpoints (single-
	// tenant homelab escape hatch). Read-only inventory and inference paths are
	// unaffected.
	if !h.allowManagement && isBlockedManagementPath(r.URL.Path) {
		log.Printf("blocked management endpoint (key=%s path=%s request_id=%s)", keyName, r.URL.Path, requestID)
		writeAPIError(w, http.StatusForbidden, "endpoint not permitted through the mesh proxy", "invalid_request_error", "endpoint_blocked")
		metrics.RequestsTotal(keyName, "", "none", "403")
		return
	}

	// Reject unsupported OpenAI endpoints with a clear 501 before routing.
	// DELETE /v1/models/{model} - model deletion is out of scope for an inference proxy.
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/models/") {
		writeAPIError(w, http.StatusNotImplemented,
			"this endpoint is not supported by marbor; for inference use /v1/chat/completions or /v1/completions",
			"invalid_request_error", "unsupported_endpoint")
		return
	}
	if isUnsupportedOpenAIPath(r.URL.Path) {
		writeAPIError(w, http.StatusNotImplemented,
			"this endpoint is not supported by marbor; for inference use /v1/chat/completions or /v1/completions",
			"invalid_request_error", "unsupported_endpoint")
		return
	}

	// Feature 3: intercept GET /v1/models and return aggregated OpenAI-schema list.
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		h.serveModels(w)
		return
	}

	// GET /v1/models/{model} - single model lookup.
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/models/") {
		modelID := strings.TrimPrefix(r.URL.Path, "/v1/models/")
		if modelID != "" {
			h.serveModel(w, modelID)
			return
		}
	}

	var body []byte
	if r.Body != nil {
		var err error
		// Bound per-request memory: a single client cannot force the proxy to
		// buffer an unbounded body. 32 MiB is generous for prompts and
		// base64-encoded images while still capping a DoS vector.
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeAPIError(w, http.StatusRequestEntityTooLarge, "request body exceeds 32MiB limit", "invalid_request_error", "request_too_large")
			} else {
				writeAPIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "read_body_error")
			}
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	modelName := router.ExtractModelName(body)

	// Enforce per-key model allow-list. An empty list means no restriction.
	// Captured in allowedModels (rather than re-derived) so the local
	// degradation chain below can re-apply the same restriction to any
	// substitute model - a key's allow-list must survive a degradation swap.
	allowedModels := auth.AllowedModelsFromContext(r.Context())
	if len(allowedModels) > 0 {
		permitted := false
		for _, m := range allowedModels {
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
			writeAPIError(w, http.StatusForbidden, fmt.Sprintf("model %q not allowed for this api key", modelName), "invalid_request_error", "model_not_allowed")
			metrics.RequestsTotal(keyName, modelName, "none", "403")
			return
		}
	}

	// VRAM-aware quantization fallback. Opt-in via routing.fallback_chains -
	// no silent auto-substitution outside a chain the operator explicitly
	// declared. Only triggers when the requested model provably does not fit
	// in free VRAM on any healthy node (real headroom data, never guessed),
	// and only substitutes an alternate that is already downloaded (never
	// triggers a fresh multi-GB download on the hot path). Pre-scoring
	// Hard-Constraint filter - does not touch weighted placement scoring.
	requestedModelName := modelName
	if chain := h.router.FallbackChainFor(modelName); len(chain) > 0 && !h.router.ModelFitsAnyHealthyNode(modelName) {
		for _, alt := range chain {
			if h.router.ModelDownloadedAnyNode(alt) && h.router.ModelFitsAnyHealthyNode(alt) {
				modelName = alt
				break
			}
		}
	}
	if modelName != requestedModelName {
		w.Header().Set("X-Marbor-Model-Fallback", requestedModelName+" -> "+modelName)
		body = rewriteModelField(body, modelName)
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	// Advanced model configuration overrides (item #20) are applied further
	// down, right after a node is selected - the profile is keyed by
	// (model, node), so which node's config applies can't be known until
	// routing has actually picked one. See the block right after
	// h.router.IncrConn(node) below.

	// Context-length vs. model-window admission check. Cheap char-count/4
	// heuristic - no tokenizer dependency. A model's context window is
	// identical across every node, so this can't discriminate routing
	// candidates and is checked here, before routing, rather than in
	// placement scoring. Only runs for models with an operator-declared
	// window (config.context_windows); an undeclared model is never guessed.
	if h.admin != nil {
		if window, ok := h.admin.ContextWindowFor(modelName); ok {
			if estTokens := len(body) / 4; estTokens > window {
				writeAPIError(w, http.StatusBadRequest,
					fmt.Sprintf("request (~%d estimated tokens) exceeds %q's %d-token context window", estTokens, modelName, window),
					"invalid_request_error", "context_length_exceeded")
				metrics.RequestsTotal(keyName, modelName, "none", "400")
				return
			}
		}
	}

	// Extract session ID for KV-cache affinity. The header is optional; an
	// absent or empty value means stateless routing (no sticky session).
	sessionID := strings.TrimSpace(r.Header.Get("X-Session-ID"))

	// Per-key opt-in for local degradation chain substitution (P67). A silent
	// model swap would be a correctness surprise for an API consumer, so a
	// request is only eligible for chain substitution when the operator has
	// granted this key that policy - declaring routing.local_degradation_chains
	// alone is not enough, and the client cannot self-authorize it (a client
	// deciding whether to degrade would defeat the point of an operator
	// policy). An unknown/anonymous key defaults to false (opted out).
	allowLocalDegradation := h.auth != nil && h.auth.IsAllowLocalDegradation(keyName)

	// Determine runtime filter from request path. Ollama-native paths (/api/*)
	// must only route to Ollama nodes; /v1/* paths can reach any backend
	// (vLLM, TGI, llama.cpp, Ollama). An empty filter means no restriction.
	runtimeFilter := ""
	if isOllamaPath(r.URL.Path) {
		runtimeFilter = "ollama"
	}

	// WaitForNode tries an immediate route first; if no node is available it
	// queues the request and blocks until a node frees up or the queue timeout
	// elapses. On timeout or queue-full it returns nil and we fall through to
	// cloud fallback or 503 as before. The request context is passed so a
	// client disconnect aborts the wait immediately.
	// WaitForNode threads the runtimeFilter through so the queue only wakes up
	// and returns nodes that match the path requirement (e.g. "ollama" for /api/*).
	// This eliminates the previous discard-and-fall-through behavior where a valid
	// Ollama node could be available but the request still hit 503 because a
	// non-Ollama node was returned and silently discarded (#3).
	node, warm, decision := h.router.WaitForNode(r.Context(), modelName, sessionID, runtimeFilter)

	// degradedOnce enforces the documented single-hop invariant across both
	// trigger sites in this method: a request may substitute at most once,
	// even if the chosen alternate has its own chain entry. Without this, a
	// chain of independently-declared entries (or a cycle) could substitute
	// repeatedly across retry-loop iterations, defeating maxRetries and, for
	// a cycle, looping the request indefinitely.
	degradedOnce := false

	if node == nil {
		// Local degradation chain (P67): opt-in, local-only substitution tried
		// before cloud egress - a degraded-but-local answer is strictly better
		// than cloud for a privacy-motivated operator. Tried before
		// localOnlyBlocked since a successful substitution never leaves local
		// nodes and so never needs to be blocked.
		if altNode, alt, altWarm, altDecision, ok := h.tryLocalDegradationChain(modelName, runtimeFilter, allowLocalDegradation, allowedModels); ok {
			body = applyLocalDegradation(w, body, modelName, alt)
			r.Body = io.NopCloser(bytes.NewReader(body))
			modelName = alt
			node = altNode
			warm = altWarm
			decision = altDecision
			degradedOnce = true
		}
	}

	if node == nil {
		if h.localOnlyBlocked(w, keyName, modelName) {
			return
		}
		// Try cloud fallback first - cloud providers support /api/* paths via
		// the translating transport, so Ollama-native clients can still reach
		// cloud when no local Ollama node is available.
		clouds := h.router.CloudChain()
		if len(clouds) > 0 {
			if h.admin != nil {
				if exceeded, reason := h.admin.CloudBudgetExceeded(keyName); exceeded {
					writeAPIError(w, http.StatusServiceUnavailable, reason, "server_error", "cloud_budget_exceeded")
					metrics.RequestsTotal(keyName, modelName, "none", "503")
					return
				}
			}
			h.proxyToCloud(w, r, body, modelName, keyName, requestID, start, clouds, 0)
			return
		}
		// No local node and no cloud. If this was an Ollama-native path, return
		// a clear actionable message so the caller knows to use /v1/ for
		// non-Ollama backends rather than getting a generic 503.
		if runtimeFilter == "ollama" {
			writeAPIError(w, http.StatusServiceUnavailable, "no Ollama nodes available; use /v1/ endpoint for non-Ollama backends", "server_error", "no_nodes_available")
			metrics.RequestsTotal(keyName, modelName, "none", "503")
			return
		}
		writeAPIError(w, http.StatusServiceUnavailable, "no healthy nodes available", "server_error", "no_nodes_available")
		metrics.RequestsTotal(keyName, modelName, "none", "503")
		return
	}

	h.router.IncrConn(node)
	h.router.RecordModelUse(node.Name, modelName) // LRU signal for model eviction

	// Advanced model configuration overrides (item #20): apply the operator's
	// configured default profile for this (model, node) pair, if one exists.
	// A profile is keyed by node as well as model - the same model name can
	// be resident on nodes with different runtimes or different VRAM
	// budgets, so it can only be resolved once a node has actually been
	// selected. rpm/tpm are enforced as a pre-send gate; every other field
	// is merged into the outgoing body only where the client didn't already
	// specify it, using injection rules specific to this node's runtime.
	var modelCfg store.ModelConfig
	var hasModelCfg bool
	if h.admin != nil {
		modelCfg, hasModelCfg = h.admin.ModelConfigFor(modelName, node.Name)
	}
	if hasModelCfg && (modelCfg.RPM != nil || modelCfg.TPM != nil) {
		if !h.modelLimiter.allow(modelName, node.Name, modelCfg.RPM, modelCfg.TPM) {
			h.router.DecrConn(node)
			if h.auth != nil {
				h.auth.Refund(keyName)
			}
			writeAPIError(w, http.StatusTooManyRequests,
				fmt.Sprintf("model %q rate limit exceeded on node %q (rpm/tpm cap)", modelName, node.Name),
				"server_error", "model_rate_limited")
			metrics.RequestsTotal(keyName, modelName, node.Name, "429")
			return
		}
	}
	if hasModelCfg {
		runtime := node.Runtime
		body = injectModelDefaults(body, runtime, modelCfg)
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	targetURL, err := url.Parse(node.URL)
	if err != nil {
		h.router.DecrConn(node)
		writeAPIError(w, http.StatusInternalServerError, "invalid node URL", "server_error", "internal_error")
		return
	}

	// Feature 2: custom transport with ResponseHeaderTimeout so a node that
	// accepts the connection but hangs (model load stall, GPU OOM) does not
	// block the client or leak goroutines. This covers only the wait for
	// response headers - NOT the streaming body - so R2 (no buffering) is safe.
	transport := h.localRoundTripper()

	// Feature 1: retry/failover loop. The ErrorHandler fires only when the
	// upstream failed before sending any response bytes, so retrying a
	// different node is safe and does not violate R2.
	tried := map[string]bool{node.URL: true}
	maxRetries := h.router.MaxRetries()

	var (
		rec     *statusRecorder
		aborted bool
	)

	// retryCount/lastFailedNode are read-only context for the P41
	// explainability Detail annotation applied after the loop - the router
	// itself stays ignorant of retry semantics (see RouteExcluding's doc
	// comment); this is purely a proxy-layer string annotation.
	retryCount := 0
	lastFailedNode := ""

	for attempt := 0; ; attempt++ {
		proxy := buildLocalProxy(targetURL, body, r, transport, requestID)

		// retryNode is set inside the ErrorHandler closure when a retry is
		// needed. Using a pointer-to-pointer lets the closure write through to
		// a variable in this stack frame without heap allocation overhead.
		var nextNode *router.NodeState
		var nextDecision *router.RoutingDecision
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
			h.router.RecordRequestOutcome(node.Name, false)

			// If the client already disconnected, do not burn an alternate node
			// or a cloud call on a request nobody is waiting for.
			if origReq.Context().Err() != nil {
				retryErr = origReq.Context().Err()
				return
			}

			if attempt < maxRetries {
				alt, _, altDecision := h.router.RouteExcluding(modelName, runtimeFilter, tried)
				if alt != nil {
					metrics.Retry(node.Name)
					lastFailedNode = node.Name
					retryCount++
					nextNode = alt
					nextDecision = altDecision
					retryErr = e
					return
				}
			}
			// No alternate nodes for the current model. Try the local
			// degradation chain (P67, opt-in) before falling through to
			// cloud - gated on !degradedOnce so a request can only ever
			// substitute once, even across retry-loop iterations (enforces
			// the single-hop invariant in code, not just in a comment, and
			// bounds a cyclic operator config to one substitution instead of
			// looping the retry loop indefinitely).
			if !degradedOnce {
				if altNode, altModel, _, altDecision, ok := h.tryLocalDegradationChain(modelName, runtimeFilter, allowLocalDegradation, allowedModels); ok {
					body = applyLocalDegradation(rw, body, modelName, altModel)
					origReq.Body = io.NopCloser(bytes.NewReader(body))
					modelName = altModel
					lastFailedNode = node.Name
					retryCount++
					nextNode = altNode
					nextDecision = altDecision
					retryErr = e
					degradedOnce = true
					return
				}
			}
			// No alternate nodes - try cloud fallback.
			if h.localOnlyBlocked(rw, keyName, modelName) {
				retryErr = errCloudHandled
				return
			}
			clouds := h.router.CloudChain()
			if len(clouds) > 0 {
				if h.admin != nil {
					if exceeded, reason := h.admin.CloudBudgetExceeded(keyName); exceeded {
						retryErr = errCloudHandled
						writeAPIError(rw, http.StatusServiceUnavailable, reason, "server_error", "cloud_budget_exceeded")
						metrics.RequestsTotal(keyName, modelName, "none", "503")
						return
					}
				}
				// Signal cloud path via sentinel before calling proxyToCloud,
				// which writes the response. The outer loop checks this after
				// serveAndRecoverAbort returns.
				retryErr = errCloudHandled
				h.proxyToCloud(rw, origReq, body, modelName, keyName, requestID, start, clouds, 0)
				return
			}
			// Log detail server-side; return a generic message so upstream
			// topology never leaks to the client.
			log.Printf("upstream error (node=%s request_id=%s): %v", node.Name, requestID, e)
			writeAPIError(rw, http.StatusBadGateway, "upstream unavailable", "server_error", "upstream_error")
		}

		rec = &statusRecorder{ResponseWriter: w, start: start}
		aborted = serveAndRecoverAbort(proxy, rec, r)

		if retryErr == errCloudHandled {
			// Cloud path handled the response and did its own logging.
			return
		}

		if nextNode != nil {
			// Switch to the alternate node and retry.
			node = nextNode
			decision = nextDecision
			h.router.IncrConn(node)
			targetURL, err = url.Parse(node.URL)
			if err != nil {
				h.router.DecrConn(node)
				writeAPIError(w, http.StatusInternalServerError, "invalid node URL", "server_error", "internal_error")
				return
			}
			warm = false // retried node may not have model warm
			continue
		}

		// Success or terminal failure - exit retry loop. Skip the decrement if
		// ErrorHandler already released this node's slot.
		if !errHandled {
			h.router.DecrConn(node)
			success := rec.StatusCode() < 500 && !aborted
			h.router.RecordRequestOutcome(node.Name, success)
		}
		break
	}

	// P41: annotate the routing explanation with retry context the router
	// itself never sees - purely a proxy-layer string addition, no change to
	// Reason/Score/Components.
	if decision != nil && retryCount > 0 {
		decision.Detail = fmt.Sprintf("%s (retry attempt %d after node %s failed)", decision.Detail, retryCount+1, lastFailedNode)
	}

	duration := time.Since(start).Seconds()
	metricStatus := rec.Status()
	if aborted {
		metricStatus = "aborted"
	}
	metrics.RequestsTotal(keyName, modelName, node.Name, metricStatus)
	metrics.RequestDuration(modelName, node.Name, duration)
	if ttft := rec.ttft(); ttft > 0 {
		metrics.RequestTTFT(modelName, node.Name, ttft.Seconds())
	}

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
		tokens := rec.tokenCount(aborted)
		clientIP := r.RemoteAddr
		if h.trustProxyHeaders {
			if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
				if parts := strings.SplitN(fwd, ",", 2); len(parts) > 0 {
					clientIP = strings.TrimSpace(parts[0])
				}
			} else if fwd2 := r.Header.Get("X-Real-IP"); fwd2 != "" {
				clientIP = fwd2
			}
		}
		loggedModel := modelName
		if modelName != requestedModelName {
			loggedModel = requestedModelName + " -> " + modelName
		}
		h.admin.LogRequest(requestID, keyName, clientIP, loggedModel, node.Name, status, rec.StatusCode(), latencyMs, tokens, decision)
		if tokens >= 0 {
			h.admin.TrackLocalRequestModel(keyName, modelName, tokens, rec.evalDurationMs())
			h.modelLimiter.recordTokens(modelName, node.Name, int64(tokens))
		}
	}
	if h.audit != nil {
		// audit_log.status is a real HTTP status code (or "aborted"), not the
		// cold/warm/error label above - the frontend's StatusBadge and the
		// server's own CAST(status AS INTEGER) success/client_error/
		// server_error filter both assume a numeric code.
		auditStatus := rec.Status()
		if aborted {
			auditStatus = "aborted"
		}
		routingReason := ""
		if decision != nil {
			routingReason = decision.Reason
		}
		h.audit.Log(audit.Entry{
			Time:          time.Now(),
			RequestID:     requestID,
			KeyName:       keyName,
			Model:         requestedModelName,
			Node:          node.Name,
			Status:        auditStatus,
			LatencyMs:     latencyMs,
			Cloud:         false,
			RoutingReason: routingReason,
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

// applyLocalDegradation records a local degradation substitution (the
// fallback header and metric) and rewrites body's model field to alt.
// Shared by both tryLocalDegradationChain call sites in ServeHTTP so the
// header format, metric call, and rewrite stay in one place.
func applyLocalDegradation(w http.ResponseWriter, body []byte, from, alt string) []byte {
	w.Header().Set("X-Marbor-Model-Fallback", from+" -> "+alt)
	metrics.LocalDegradation(from, alt)
	return rewriteModelField(body, alt)
}

// tryLocalDegradationChain walks the operator-declared local degradation
// chain for modelName (routing.local_degradation_chains), in order, probing
// each candidate with a single non-blocking RouteExcluding call. This never
// queues behind an unavailable alternate - by design, only the non-blocking
// call is used here, never WaitForNode. RouteExcluding (not Route) is used
// deliberately: it never reads or writes session affinity, so degrading to
// an alternate model can never pin the session's sticky node to a node
// chosen for the wrong model. No node exclusion set is applied here - a node
// that just failed to serve modelName may still be perfectly healthy for an
// alternate model (the common single-node fleet case), so the retry loop's
// per-model "tried" set must not carry over to a model switch. allowedModels
// is the caller's per-key model allow-list (nil/empty means unrestricted); a
// candidate the key is not permitted to use is skipped, never substituted.
// Callers must enforce single-hop themselves (never call this again within
// the same request after a successful substitution) - this function only
// walks one level of one chain per call. Returns ok=false if allowDegradation
// is false, no chain is declared for modelName, or every eligible candidate
// is currently unavailable.
func (h *Handler) tryLocalDegradationChain(modelName, runtimeFilter string, allowDegradation bool, allowedModels []string) (node *router.NodeState, alt string, warm bool, decision *router.RoutingDecision, ok bool) {
	if !allowDegradation {
		return nil, "", false, nil, false
	}
	for _, candidate := range h.router.LocalDegradationChainFor(modelName) {
		if len(allowedModels) > 0 && !slices.Contains(allowedModels, candidate) {
			continue
		}
		if n, w, d := h.router.RouteExcluding(candidate, runtimeFilter, nil); n != nil {
			return n, candidate, w, d, true
		}
	}
	return nil, "", false, nil, false
}

// errCloudHandled is a sentinel used inside the ErrorHandler closure to signal
// that the cloud fallback path has already written the response.
var errCloudHandled = fmt.Errorf("cloud handled")

// buildLocalProxy constructs a reverse proxy for the given node URL.
// It is extracted so the retry loop can rebuild it cleanly for each attempt.
func buildLocalProxy(targetURL *url.URL, body []byte, orig *http.Request, transport *http.Transport, requestID string) *httputil.ReverseProxy {
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
			// Authorization is the mesh's own API-key credential; Cookie carries
			// the admin dashboard's httpOnly session cookie if this request came
			// via a browser proxy path. Neither belongs on a backend GPU node.
			if k != "Authorization" && k != "Cookie" {
				req.Header[k] = v
			}
		}
		// Forward request ID to upstream so mesh and Ollama logs correlate.
		if requestID != "" {
			req.Header.Set("X-Request-ID", requestID)
		}
		if len(body) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
	}
	return proxy
}

// isUnsupportedOpenAIPath returns true for OpenAI API paths that are out of
// scope for an inference proxy. These return 501 instead of falling through to
// Ollama, which would return a wrong-shape or confusing error.
// Each entry is checked both as an exact match and as a path prefix (with
// trailing slash) so that /v1/files and /v1/files/upload both match.
func isUnsupportedOpenAIPath(path string) bool {
	unsupported := []string{
		"/v1/images",
		"/v1/audio",
		"/v1/fine-tuning",
		"/v1/files",
		"/v1/assistants",
		"/v1/threads",
		"/v1/batches",
		"/v1/vector-stores",
	}
	for _, base := range unsupported {
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return path == "/v1/moderations"
}

// modelStatus is the status field added to model entries (ignored by OpenAI clients).
type modelStatus = string

const (
	modelStatusLoaded    modelStatus = "loaded"
	modelStatusAvailable modelStatus = "available"
)

// serveModels handles GET /v1/models by returning an OpenAI-schema list of
// ALL models available across healthy nodes: both models currently in VRAM
// (from /api/ps polling) and models downloaded but not warm (from /api/tags).
// A "status" field distinguishes loaded vs available; OpenAI clients ignore it.
func (h *Handler) serveModels(w http.ResponseWriter) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		Created int64  `json:"created"`
		Status  string `json:"status"`
	}

	// seen tracks best status per model name: loaded beats available.
	seen := make(map[string]string) // name -> status
	for _, n := range h.router.Nodes() {
		n.RLock()
		healthy := n.Healthy
		loaded := n.LoadedModels
		nodeURL := n.URL
		n.RUnlock()
		if !healthy {
			continue
		}
		// Warm models (in VRAM).
		for _, m := range loaded {
			seen[m.Name] = modelStatusLoaded
		}
		// Downloaded models from /api/tags (catalog). FetchModelTags uses a
		// 30-second cache so this is cheap on repeated calls.
		tags, err := h.router.FetchModelTags(nodeURL)
		if err == nil {
			for _, t := range tags {
				if _, exists := seen[t.Name]; !exists {
					seen[t.Name] = modelStatusAvailable
				}
			}
		}
	}

	// Stable sort for deterministic output.
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	type response struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}

	now := time.Now().Unix()
	data := make([]modelEntry, 0, len(names))
	for _, name := range names {
		data = append(data, modelEntry{
			ID:      name,
			Object:  "model",
			OwnedBy: "marbor",
			Created: now,
			Status:  seen[name],
		})
	}

	out, _ := json.Marshal(response{Object: "list", Data: data})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out) //nolint:errcheck
}

// serveModel handles GET /v1/models/{model} - single model lookup across all
// healthy nodes. Checks loaded models first, then the /api/tags catalog.
func (h *Handler) serveModel(w http.ResponseWriter, modelID string) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		Created int64  `json:"created"`
		Status  string `json:"status"`
	}

	status := ""
	for _, n := range h.router.Nodes() {
		n.RLock()
		healthy := n.Healthy
		loaded := n.LoadedModels
		nodeURL := n.URL
		n.RUnlock()
		if !healthy {
			continue
		}
		for _, m := range loaded {
			if m.Name == modelID {
				status = modelStatusLoaded
				break
			}
		}
		if status == modelStatusLoaded {
			break
		}
		// Check catalog (downloaded but not warm).
		tags, err := h.router.FetchModelTags(nodeURL)
		if err == nil {
			for _, t := range tags {
				if t.Name == modelID {
					status = modelStatusAvailable
					break
				}
			}
		}
	}

	if status == "" {
		writeAPIError(w, http.StatusNotFound,
			"model '"+modelID+"' not found",
			"invalid_request_error", "model_not_found")
		return
	}

	out, _ := json.Marshal(modelEntry{
		ID:      modelID,
		Object:  "model",
		OwnedBy: "marbor",
		Created: time.Now().Unix(),
		Status:  status,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out) //nolint:errcheck
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

func (h *Handler) proxyToCloud(w http.ResponseWriter, r *http.Request, body []byte, modelName, keyName, requestID string, start time.Time, clouds []config.CloudProvider, idx int) {
	cloud := &clouds[idx]
	hasNext := idx+1 < len(clouds)
	delegated := false
	metrics.CloudFallback(cloud.Name)
	path := translateCloudPath(r.URL.Path)

	// Anthropic has no embeddings endpoint. Chat/completions requests are
	// translated to Anthropic's native /v1/messages schema by
	// anthropicTransport below; embeddings has no equivalent to translate to,
	// so return a clear 501 before the request leaves the mesh.
	if strings.EqualFold(cloud.Provider, "anthropic") && path == "/v1/embeddings" {
		if hasNext {
			h.proxyToCloud(w, r, body, modelName, keyName, requestID, start, clouds, idx+1)
			return
		}
		writeAPIError(w, http.StatusNotImplemented,
			"the Anthropic cloud provider does not support "+path+" through marbor; use an OpenAI-compatible overflow provider for this endpoint",
			"invalid_request_error", "unsupported_cloud_endpoint")
		metrics.RequestsTotal(keyName, modelName, "cloud:"+cloud.Name, "501")
		return
	}

	outBody := body
	// Ollama's legacy /api/embeddings request uses "prompt"; OpenAI's
	// /v1/embeddings (translateCloudPath's target for this path) expects
	// "input". Rewrite before the model-field rewrite below so both compose.
	if r.URL.Path == "/api/embeddings" && len(outBody) > 0 {
		outBody = rewritePromptToInput(outBody)
	}
	// loggedModel makes model rewriting visible: "<original> -> <cloud model>"
	// in the request log when the cloud provider's default_model replaced the
	// client's requested model, plain "<original>" otherwise.
	loggedModel := modelName
	cloudModel := ""
	if cloud.DefaultModel != "" && len(outBody) > 0 {
		outBody = rewriteModelField(outBody, cloud.DefaultModel)
		cloudModel = cloud.DefaultModel
		loggedModel = modelName + " -> " + cloud.DefaultModel
	}

	targetURL, err := url.Parse(cloud.BaseURL)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "invalid cloud provider URL", "server_error", "internal_error")
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	// Bound the cloud header phase with a dedicated transport (shared, built
	// once) so a hung provider does not leak goroutines/connections. Only the
	// header wait is bounded - the streaming body is untouched, so R2 holds.
	cloudTransport := h.cloudRoundTripper()
	var transport http.RoundTripper = cloudTransport
	// Anthropic only exposes /v1/messages. Insert the Anthropic translator
	// between the shared cloud transport and everything above it, so the
	// rest of the pipeline keeps working against the OpenAI shape it already
	// understands - it rewrites the outbound request into the Messages
	// schema and rewrites the response back into OpenAI shape.
	if strings.EqualFold(cloud.Provider, "anthropic") {
		transport = &anthropicTransport{inner: cloudTransport, apiKey: cloud.APIKey}
	}
	proxy.Transport = transport
	// When the original client path is Ollama-native (/api/chat or
	// /api/generate) wrap the transport to translate the OpenAI response back
	// into Ollama NDJSON. For /v1/... paths the cloud response passes through
	// unchanged (current behavior preserved). The translating wrapper delegates
	// to the same (possibly Anthropic-translating) transport via its inner
	// round-tripper.
	if isOllamaPath(r.URL.Path) {
		proxy.Transport = &translatingTransport{
			inner:        transport,
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
		// Cookie carries the admin dashboard's httpOnly session cookie if this
		// request came via a browser proxy path - never send it to a cloud provider.
		req.Header.Del("Cookie")
		req.Header.Set("Authorization", "Bearer "+cloud.APIKey)
		if len(outBody) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(outBody))
			req.ContentLength = int64(len(outBody))
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, outReq *http.Request, err error) {
		// Log the detailed error server-side, but never leak upstream topology
		// (hostnames, ports, dial/TLS details) to the client.
		log.Printf("cloud upstream error (provider=%s request_id=%s): %v", cloud.Name, requestID, err)
		if hasNext {
			delegated = true
			// Use the original, unmutated request r (not outReq, which is the
			// Director-rewritten outbound request) so the fallback provider
			// sees the client's real path when deciding on Ollama translation.
			h.proxyToCloud(w, r, body, modelName, keyName, requestID, start, clouds, idx+1)
			return
		}
		writeAPIError(w, http.StatusBadGateway, "upstream unavailable", "server_error", "upstream_error")
	}

	rec := &statusRecorder{ResponseWriter: w, start: start}
	aborted := serveAndRecoverAbort(proxy, rec, r)
	if delegated {
		return
	}

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
	if ttft := rec.ttft(); ttft > 0 {
		metrics.RequestTTFT(modelName, nodeName, ttft.Seconds())
	}

	if h.admin != nil {
		latencyMs := int(time.Since(start).Milliseconds())
		tokens := rec.tokenCount(aborted)
		logTokens := tokens
		if logTokens < 0 {
			logTokens = 0
		}
		clientIP := r.RemoteAddr
		if h.trustProxyHeaders {
			if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
				if parts := strings.SplitN(fwd, ",", 2); len(parts) > 0 {
					clientIP = strings.TrimSpace(parts[0])
				}
			} else if fwd2 := r.Header.Get("X-Real-IP"); fwd2 != "" {
				clientIP = fwd2
			}
		}
		h.admin.LogRequest(requestID, keyName, clientIP, loggedModel, nodeName, status, rec.StatusCode(), latencyMs, logTokens, nil)
		if tokens >= 0 {
			h.admin.TrackCloudCostModel(keyName, cloud.Name, modelName, cloud.CostPer1KTokens, tokens)
			// Model-config rpm/tpm caps are keyed to a specific local mesh
			// node's profile and don't apply to cloud dispatch - cloud spend
			// already has its own governance via CloudBudgetExceeded and the
			// runtime_keys daily/monthly USD caps, which is the correct place
			// for cloud cost control rather than a per-node capacity knob.
		}
	}
	if h.audit != nil {
		// See the matching comment in the local path above: audit_log.status
		// must be a real HTTP status code (or "aborted"), and metricStatus
		// (computed above for Prometheus) already is.
		h.audit.Log(audit.Entry{
			Time:       time.Now(),
			RequestID:  requestID,
			KeyName:    keyName,
			Model:      modelName,
			Node:       nodeName,
			Status:     metricStatus,
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
	case "/api/embed":
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

// rewritePromptToInput renames the legacy Ollama /api/embeddings request's
// "prompt" field to "input" for outbound cloud requests, matching OpenAI's
// /v1/embeddings request schema. No-op if "prompt" is absent or "input" is
// already present.
func rewritePromptToInput(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	prompt, hasPrompt := m["prompt"]
	if !hasPrompt {
		return body
	}
	if _, hasInput := m["input"]; !hasInput {
		m["input"] = prompt
	}
	delete(m, "prompt")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode  int
	tail        []byte    // retained body for token-count parsing - see tailMax
	sawNewline  bool      // true once a '\n' has appeared in the written body
	start       time.Time // request start, for TTFT; zero value means TTFT is unavailable
	firstByteAt time.Time // set on the first Write(); zero until then
}

// tailMax bounds the retained response tail for line-oriented responses -
// Ollama NDJSON (one JSON object per line) or OpenAI SSE ("data: " lines) -
// where the token count lives in the final line, so a small tail is enough.
//
// A single-JSON-document response (e.g. /v1/embeddings, whose "usage" field
// trails a large embedding array with no newline anywhere in the body) can't
// be identified by "final line" at all, so Write does not truncate until it
// has seen at least one '\n' - see sawNewline. Until then the buffer grows up
// to maxRequestBodyBytes, matching the size the proxy already accepts on the
// request side. Writes still pass straight through in both cases - streaming
// to the client is never buffered (R2).
const tailMax = 8192

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	if n > 0 {
		if r.firstByteAt.IsZero() {
			r.firstByteAt = time.Now()
		}
		if !r.sawNewline && bytes.IndexByte(b[:n], '\n') >= 0 {
			r.sawNewline = true
		}
		if !r.sawNewline {
			if len(r.tail) < maxRequestBodyBytes {
				r.tail = append(r.tail, b[:n]...)
			}
		} else {
			r.tail = append(r.tail, b[:n]...)
			if len(r.tail) > tailMax {
				r.tail = r.tail[len(r.tail)-tailMax:]
			}
		}
	}
	return n, err
}

// ttft returns time-to-first-byte: the real wall-clock gap between request
// start and the first response byte written. Returns 0 (unavailable) if no
// byte was ever written (immediate error response, or start was never set).
func (r *statusRecorder) ttft() time.Duration {
	if r.start.IsZero() || r.firstByteAt.IsZero() {
		return 0
	}
	return r.firstByteAt.Sub(r.start)
}

// tokenCount parses real token usage from the response tail. It scans lines
// from the end looking for Ollama's final object (eval_count +
// prompt_eval_count) or an OpenAI-style usage block (total_tokens).
// Returns 0 when no count is present on a normally-completed response.
// Returns -1 when aborted is true and no count is present - the terminal
// chunk carrying the real count was never sent by upstream, so this is a
// genuinely unknown value, not a real zero-token response; callers must
// skip cost/analytics accumulation for -1 rather than storing it as 0.
// Also returns -1 for Ollama's legacy /api/embeddings response shape
// ({"embedding":[...]}, no eval_count/prompt_eval_count/usage field) - that
// endpoint genuinely reports no token count, so 0 would be a fake zero (R1).
func (r *statusRecorder) tokenCount(aborted bool) int64 {
	lines := bytes.Split(r.tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		line = bytes.TrimPrefix(line, []byte("data: ")) // SSE framing
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var t struct {
			EvalCount       int64           `json:"eval_count"`
			PromptEvalCount int64           `json:"prompt_eval_count"`
			Embedding       json.RawMessage `json:"embedding"`
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
		// Ollama's legacy singular /api/embeddings response is
		// {"embedding":[...]} with no eval_count/prompt_eval_count/usage
		// field at all - there is genuinely no token count available from
		// this response shape, so this is unavailable (-1), not a real
		// zero-token measurement (R1: never present a fake zero as real).
		if t.Embedding != nil {
			return -1
		}
	}
	if aborted {
		return -1
	}
	return 0
}

// evalDurationMs parses Ollama's real eval_duration (nanoseconds spent
// generating completion tokens, excluding prompt processing) from the
// response tail. This is only present on Ollama-native responses - cloud
// providers don't report it - so it returns 0 (unavailable) for anything
// else, including OpenAI usage blocks. 0 always means "not present": unlike
// tokenCount, a genuine eval_duration is never 0, so no aborted-vs-zero
// sentinel distinction is needed here.
func (r *statusRecorder) evalDurationMs() int64 {
	lines := bytes.Split(r.tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		line = bytes.TrimPrefix(line, []byte("data: ")) // SSE framing
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var t struct {
			EvalDuration int64 `json:"eval_duration"`
		}
		if err := json.Unmarshal(line, &t); err != nil {
			continue
		}
		if t.EvalDuration > 0 {
			return t.EvalDuration / int64(time.Millisecond)
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

// StatusCode returns the real numeric HTTP status written to the client,
// defaulting to 200 when WriteHeader was never called explicitly (matching
// the standard library's own default-to-200 behavior).
func (r *statusRecorder) StatusCode() int {
	if r.statusCode == 0 {
		return 200
	}
	return r.statusCode
}

func (r *statusRecorder) Status() string {
	return fmt.Sprintf("%d", r.StatusCode())
}
