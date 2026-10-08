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
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
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
)

// maxRequestBodyBytes bounds how much of a request body the proxy will buffer
// before extracting the model name. Caps a memory-exhaustion DoS vector.
const maxRequestBodyBytes = 32 << 20 // 32 MiB

// maxModelNameBytes bounds the model name a request may carry. Real model
// names are short; an unbounded one would be copied into metrics labels,
// logs and the audit trail.
const maxModelNameBytes = 256

// localDialTimeout bounds the TCP connect to a local node.
const localDialTimeout = 5 * time.Second

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
	metrics.RequestsTotal(keyName, h.metricModel(modelName), "none", "503")
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
	// only the wait for response headers, never the streaming body, so
	// streaming stays streaming.
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
// (never the streaming body), so streaming stays streaming.
func (h *Handler) localRoundTripper() *http.Transport {
	h.localTransportOnce.Do(func() {
		h.localTransport = &http.Transport{
			// A node that accepts nothing (powered off, firewalled) must
			// fail the dial promptly so the retry loop can move on, instead
			// of waiting out the OS connect timeout.
			DialContext: (&net.Dialer{
				Timeout:   localDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   16,
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
	// Trailing slashes are ignored: a backend that serves "/api/pull/" the same
	// as "/api/pull" must not be reachable through the slash form.
	path = strings.TrimRight(path, "/")
	if _, ok := blockedManagementPaths[path]; ok {
		return true
	}
	return strings.HasPrefix(path, "/api/blobs/")
}

// SetTrustProxyHeaders toggles whether X-Forwarded-For/X-Real-IP are trusted
// for the admin request log's client IP. Pass true only when marbor sits
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

	// Canonicalise the path before any path-based decision. Without this,
	// "/api/../api/pull" or "//api/pull" slip past the exact-match management
	// guard below yet are forwarded verbatim to a backend that normalises them.
	// The cleaned path is also what gets forwarded.
	if cleaned := canonicalPath(r.URL.Path); cleaned != r.URL.Path {
		log.Printf("request path rewritten (key=%q request_id=%q from=%q to=%q)", keyName, requestID, truncateForLog(r.URL.Path, 128), truncateForLog(cleaned, 128))
		r.URL.Path = cleaned
		r.URL.RawPath = ""
	}

	// Default-deny management-endpoint guard. Destructive Ollama management
	// paths (/api/delete, /api/pull, /api/push, /api/create, /api/copy,
	// /api/blobs[/...]) are blocked before any routing/forwarding so an
	// authenticated inference key cannot mutate models on shared backend nodes.
	// Overridable per-deploy via routing.allow_management_endpoints (single-
	// tenant homelab escape hatch). Read-only inventory and inference paths are
	// unaffected.
	if !h.allowManagement && isBlockedManagementPath(r.URL.Path) {
		log.Printf("blocked management endpoint (key=%q path=%q request_id=%q)", keyName, truncateForLog(r.URL.Path, 128), requestID)
		writeAPIError(w, http.StatusForbidden, "endpoint not permitted through the marbor proxy", "invalid_request_error", "endpoint_blocked")
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

	// Request bodies are read, inspected and forwarded as plain bytes; a
	// compressed one would be parsed as garbage here and, on a cloud handoff,
	// sent on with its Content-Encoding header dropped. Refuse it plainly.
	if enc := strings.TrimSpace(r.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		writeAPIError(w, http.StatusUnsupportedMediaType, "compressed request bodies are not supported", "invalid_request_error", "unsupported_content_encoding")
		metrics.RequestsTotal(keyName, "", "none", "415")
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

	// The allow-list and routing read the model name through
	// router.ExtractModelName, which matches the key case-insensitively and
	// takes the last duplicate, while a backend may read a different one. A body
	// whose model key is ambiguous is rejected so every parser sees the same name.
	if problem := modelFieldProblem(body); problem != "" {
		log.Printf("rejected request (key=%q request_id=%q reason=%q)", keyName, requestID, problem)
		writeAPIError(w, http.StatusBadRequest, "request body rejected: "+problem, "invalid_request_error", "invalid_model")
		metrics.RequestsTotal(keyName, "", "none", "400")
		return
	}

	modelName := router.ExtractModelName(body)
	if len(modelName) > maxModelNameBytes {
		log.Printf("rejected request (key=%q request_id=%q reason=%q model=%q)", keyName, requestID, "model name too long", truncateForLog(modelName, 64))
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("model name exceeds %d bytes", maxModelNameBytes), "invalid_request_error", "invalid_model")
		metrics.RequestsTotal(keyName, "", "none", "400")
		return
	}

	// Model alias resolution. An operator-declared alias (e.g. "gpt-4") is
	// rewritten to its real target model before anything else looks at the
	// model name, so fallback chains, context-window admission, prefix
	// locality, session affinity, routing, model configs, rate limits,
	// metrics and analytics all key on the real model. clientModelName keeps
	// the name the client actually sent, for the allow-list, the request log,
	// and cloud fallback (a cloud provider knows "gpt-4", not a local name).
	// Only the already-buffered request body is rewritten; the response
	// stream is untouched. Management paths (pull, delete, copy, ...) are
	// never aliased: those name a model on disk, not a request to serve one.
	clientModelName := modelName
	aliased := false
	if !isBlockedManagementPath(r.URL.Path) {
		if target, ok := h.router.ResolveModelAlias(modelName); ok {
			modelName = target
			aliased = true
			body = rewriteModelField(body, target)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
	}

	// Enforce per-key model allow-list. An empty list means no restriction.
	// Captured in allowedModels (rather than re-derived) so the local
	// degradation chain below can re-apply the same restriction to any
	// substitute model - a key's allow-list must survive a degradation swap.
	allowedModels := auth.AllowedModelsFromContext(r.Context())
	if len(allowedModels) > 0 {
		// For an aliased request, listing either the alias name the client
		// sent or the real model it resolves to is enough.
		permitted := slices.Contains(allowedModels, modelName) ||
			(aliased && slices.Contains(allowedModels, clientModelName))
		if !permitted {
			// The request is rejected by policy before reaching any node, so
			// refund the rate-limit token and quota count auth consumed - a
			// disallowed model must not burn the key's budget.
			if h.auth != nil {
				h.auth.Refund(keyName)
			}
			// Name only what the client sent - never reveal an alias's target
			// to a key that is not allowed to use it.
			writeAPIError(w, http.StatusForbidden, fmt.Sprintf("model %q not allowed for this api key", clientModelName), "invalid_request_error", "model_not_allowed")
			// A fixed label: the model name here is client-chosen, and an
			// unauthorised key must not be able to mint metric series.
			metrics.RequestsTotal(keyName, "disallowed", "none", "403")
			return
		}
	}
	// The resolution header is set only once the key is known to be allowed,
	// so a rejected request never learns what the alias points to.
	if aliased {
		w.Header().Set("X-Marbor-Model-Alias", clientModelName+" -> "+modelName)
	}

	// VRAM-aware quantization fallback. Opt-in via routing.fallback_chains -
	// no silent auto-substitution outside a chain the operator explicitly
	// declared. Only triggers when the requested model provably does not fit
	// in free VRAM on any healthy node (real headroom data, never guessed),
	// and only substitutes an alternate that is already downloaded (never
	// triggers a fresh multi-GB download on the hot path). Pre-scoring
	// Hard-Constraint filter - does not touch weighted placement scoring.
	// Uses the same cheap char-count/4 heuristic as the context-window
	// admission check further down (no tokenizer dependency) - gives the fit check a
	// real per-request context-length signal instead of comparing disk size
	// alone, so a large-context request against a tight-headroom node is no
	// longer treated identically to a small one.
	requestedCtxTokens := int64(len(body) / 4)
	requestedModelName := modelName
	if chain := h.router.FallbackChainFor(modelName); len(chain) > 0 && !h.router.ModelFitsAnyHealthyNode(modelName, requestedCtxTokens) {
		for _, alt := range chain {
			// A key's model allow-list must survive a fallback swap, same as
			// the degradation chain: never substitute a model the key may not use.
			// Unlike the request check above, only the alternate's own name
			// counts: an alias names what the client asked for, and an
			// alternate is a real model chosen by marbor with no alias.
			if len(allowedModels) > 0 && !slices.Contains(allowedModels, alt) {
				continue
			}
			if h.router.ModelDownloadedAnyNode(alt) && h.router.ModelFitsAnyHealthyNode(alt, requestedCtxTokens) {
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
				metrics.RequestsTotal(keyName, h.metricModel(modelName), "none", "400")
				return
			}
		}
	}

	// Extract session ID for KV-cache affinity. The header is optional; an
	// absent or empty value means stateless routing (no sticky session).
	sessionID := strings.TrimSpace(r.Header.Get("X-Session-ID"))

	// Per-key opt-in for local degradation chain substitution. A silent
	// model swap would be a correctness surprise for an API consumer, so a
	// request is only eligible for chain substitution when the operator has
	// granted this key that policy - declaring routing.local_degradation_chains
	// alone is not enough, and the client cannot self-authorize it (a client
	// deciding whether to degrade would defeat the point of an operator
	// policy). An unknown/anonymous key defaults to false (opted out).
	allowLocalDegradation := h.auth != nil && h.auth.IsAllowLocalDegradation(keyName)

	// Rolling prefix-locality routing lookup: computed off the body already
	// buffered above - zero new body reads, so streaming stays streaming
	// (that guard covers the response stream, not this already-buffered
	// request body). Uses the final (possibly fallback-substituted)
	// modelName, since that's the model whose node/warm-state affinity
	// actually matters for routing, and runs BEFORE injectModelDefaults
	// mutates the body further below (that mutation is a per-node config
	// overlay applied after a node is already chosen, not part of the
	// client-sent conversation this lookup keys on). PrefixLocalityLookup
	// itself no-ops (returns "", "") when the feature is disabled or the
	// body doesn't resolve to a hashable conversation (multimodal, malformed,
	// empty) - no branching needed here. prefixRecordKey is threaded through
	// to the single completion-time record call below; it is never re-derived
	// from a body that injectModelDefaults or a later degradation swap may
	// have since mutated.
	prefixRecordKey, prefixPreferredNode := h.router.PrefixLocalityLookup(r.Context(), modelName, body)
	// lookupModel snapshots the model the key above was actually computed
	// against. A later local-degradation substitution can reassign modelName
	// to a fallback model for the rest of this request; recording under the
	// original key once a different model actually served the response would
	// leave a locality hint pointing at the wrong model, so recording below
	// only happens when the serving model still matches this snapshot.
	lookupModel := modelName

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
	node, warm, decision := h.router.WaitForNodeWithPrefix(r.Context(), modelName, sessionID, runtimeFilter, prefixPreferredNode)

	// degradedOnce enforces the documented single-hop invariant across both
	// trigger sites in this method: a request may substitute at most once,
	// even if the chosen alternate has its own chain entry. Without this, a
	// chain of independently-declared entries (or a cycle) could substitute
	// repeatedly across retry-loop iterations, defeating maxRetries and, for
	// a cycle, looping the request indefinitely.
	degradedOnce := false

	if node == nil {
		// Local degradation chain: opt-in, local-only substitution tried
		// before cloud egress - a degraded-but-local answer is strictly better
		// than cloud for a privacy-motivated operator. Tried before
		// localOnlyBlocked since a successful substitution never leaves local
		// nodes and so never needs to be blocked.
		if altNode, alt, altWarm, altDecision, ok := h.tryLocalDegradationChain(modelName, runtimeFilter, allowLocalDegradation, allowedModels, nil); ok {
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
					metrics.RequestsTotal(keyName, h.metricModel(modelName), "none", "503")
					return
				}
			}
			// The cloud provider is sent the requested name, not the alias
			// target, so the local resolution header no longer applies.
			w.Header().Del("X-Marbor-Model-Alias")
			w.Header().Del("X-Marbor-Model-Fallback")
			cloudBody, cloudModel := cloudRequestFor(body, modelName, clientModelName, aliased)
			h.proxyToCloud(w, r, cloudBody, cloudModel, keyName, requestID, start, clouds, 0)
			return
		}
		// No local node and no cloud. If this was an Ollama-native path, return
		// a clear actionable message so the caller knows to use /v1/ for
		// non-Ollama backends rather than getting a generic 503.
		if runtimeFilter == "ollama" {
			writeAPIError(w, http.StatusServiceUnavailable, "no Ollama nodes available; use /v1/ endpoint for non-Ollama backends", "server_error", "no_nodes_available")
			metrics.RequestsTotal(keyName, h.metricModel(modelName), "none", "503")
			return
		}
		writeAPIError(w, http.StatusServiceUnavailable, "no healthy nodes available", "server_error", "no_nodes_available")
		metrics.RequestsTotal(keyName, h.metricModel(modelName), "none", "503")
		return
	}

	// holding tracks whether this request currently owns a connection slot and
	// an in-flight model count on heldNode/heldModel. A deferred release
	// returns them if a panic unwinds this method, so a crash can never leak
	// the counters permanently (a leaked count skews placement forever).
	var (
		holding   bool
		heldNode  *router.NodeState
		heldModel string
	)
	acquire := func(n *router.NodeState, model string) {
		h.router.IncrConn(n)
		h.router.IncrModelInFlight(n, model)
		holding, heldNode, heldModel = true, n, model
	}
	release := func() {
		if holding {
			holding = false
			h.router.DecrConn(heldNode)
			h.router.DecrModelInFlight(heldNode, heldModel)
		}
	}
	defer release()

	acquire(node, modelName)
	h.router.RecordModelUse(node.Name, modelName) // LRU signal for model eviction
	// initialNode/initialModel let the post-retry-loop code below detect
	// whether the request ended up served by a different node/model
	// than this initial selection (failover retry, or a local-degradation
	// substitution), so it can refresh the LRU/warmth signal for whichever
	// node/model actually served the request instead of only the one
	// initially picked.
	initialNode, initialModel := node.Name, modelName

	// Advanced model configuration overrides (item #20): apply the operator's
	// configured default profile for this (model, node) pair, if one exists.
	// A profile is keyed by node as well as model - the same model name can
	// be resident on nodes with different runtimes or different VRAM
	// budgets, so it can only be resolved once a node has actually been
	// selected. rpm/tpm are enforced as a pre-send gate; every other field
	// is merged into the outgoing body only where the client didn't already
	// specify it, using injection rules specific to this node's runtime.
	//
	// baseBody is the request as the client sent it (after alias, fallback
	// and degradation rewrites) with no node-specific profile applied. Every
	// time the request moves to a different node or model - failover retry or
	// degradation - the profile and the rpm/tpm gate are re-applied from
	// baseBody for that node, so a retried request never carries the previous
	// node's injected options or skips the new node's limits. baseBody is also
	// what a cloud handoff sends, since a node profile means nothing to a cloud
	// provider.
	baseBody := body
	// profileFor applies node n's profile for model to baseBody. ok=false means
	// n's rpm/tpm cap for model is exhausted.
	profileFor := func(base []byte, n *router.NodeState, model string) (out []byte, ok bool) {
		if h.admin == nil {
			return base, true
		}
		cfg, has := h.admin.ModelConfigFor(model, n.Name)
		if !has {
			return base, true
		}
		if (cfg.RPM != nil || cfg.TPM != nil) && !h.modelLimiter.allow(model, n.Name, cfg.RPM, cfg.TPM) {
			return nil, false
		}
		return injectModelDefaults(base, n.Runtime, cfg), true
	}
	// rejectModelRateLimited answers the first-chosen node's exhausted cap with
	// a 429. The node is named only in the server log: a client has no
	// business learning fleet topology from an error body. Nothing was served,
	// so a fallback header set earlier is withdrawn.
	rejectModelRateLimited := func(rw http.ResponseWriter, n *router.NodeState, model string) {
		release()
		log.Printf("model rate limit exceeded (key=%q request_id=%q node=%q model=%q)", keyName, requestID, n.Name, truncateForLog(model, 64))
		if h.auth != nil {
			h.auth.Refund(keyName)
		}
		rw.Header().Del("X-Marbor-Model-Fallback")
		writeAPIError(rw, http.StatusTooManyRequests,
			"model rate limit exceeded (rpm/tpm cap)",
			"server_error", "model_rate_limited")
		metrics.RequestsTotal(keyName, h.metricModel(model), n.Name, "429")
	}
	var profiled bool
	if body, profiled = profileFor(baseBody, node, modelName); !profiled {
		rejectModelRateLimited(w, node, modelName)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	targetURL, err := url.Parse(node.URL)
	if err != nil {
		release()
		writeAPIError(w, http.StatusInternalServerError, "invalid node URL", "server_error", "internal_error")
		return
	}

	// Feature 2: custom transport with ResponseHeaderTimeout so a node that
	// accepts the connection but hangs (model load stall, GPU OOM) does not
	// block the client or leak goroutines. This covers only the wait for
	// response headers - NOT the streaming body - so no buffering happens and
	// streaming stays streaming.
	transport := h.localRoundTripper()

	// Feature 1: retry/failover loop. The ErrorHandler fires only when the
	// upstream failed before sending any response bytes, so retrying a
	// different node is safe and does not break streaming.
	tried := map[string]bool{node.URL: true}
	maxRetries := h.router.MaxRetries()

	var (
		rec     *statusRecorder
		aborted bool
		// streamAborted is true only when the upstream died mid-body (the
		// reverse proxy panicked http.ErrAbortHandler); aborted additionally
		// covers a client that left before any response was written.
		streamAborted bool
	)

	// retryCount/lastFailedNode are read-only context for the retry
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
		var nextBody []byte
		var nextDecision *router.RoutingDecision
		var retryErr error
		// clientDisconnected is set when ErrorHandler detects the client
		// already left before any response was written - no WriteHeader call
		// follows, so rec.statusCode stays 0 and would otherwise be reported
		// as a fabricated 200 by StatusCode()'s unset-maps-to-200 fallback.
		var clientDisconnected bool
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
			release()
			errHandled = true
			tried[node.URL] = true

			// If the client already disconnected, do not burn an alternate node
			// or a cloud call on a request nobody is waiting for - and do not
			// blame the node: a cancelled client is not a node failure, so this
			// check comes before the failure is recorded.
			if origReq.Context().Err() != nil {
				retryErr = origReq.Context().Err()
				clientDisconnected = true
				return
			}
			h.router.RecordRequestOutcome(node.Name, false)
			// The failure is logged here, once, so it is visible whichever way
			// the request goes next (retry, degradation, cloud, or a 502).
			log.Printf("upstream error (node=%q request_id=%q): %v", node.Name, requestID, e)

			if attempt < maxRetries {
				// Same modelName == lookupModel guard RecordPrefixLocality applies
				// below - prefixPreferredNode was resolved against the ORIGINAL
				// model, and a local-degradation swap may have since reassigned
				// modelName to a fallback model with no relevant KV-cache
				// relationship to that hint. Passing "" here (not the stale hint)
				// once modelName has drifted keeps this call's soft signal
				// meaningful for whichever model it is now actually retrying.
				retryPreferredNode := prefixPreferredNode
				if modelName != lookupModel {
					retryPreferredNode = ""
				}
				for {
					alt, _, altDecision := h.router.RouteExcludingWithPrefix(modelName, runtimeFilter, tried, retryPreferredNode)
					if alt == nil {
						break
					}
					// A failover node gets its own profile and rpm/tpm gate. One
					// whose cap is exhausted counts as tried, so the next
					// candidate (or degradation, cloud, or the 502) is used and
					// the client sees the original failure, not a 429 about a
					// node it never chose.
					altBody, ok := profileFor(baseBody, alt, modelName)
					if !ok {
						tried[alt.URL] = true
						log.Printf("failover node skipped: model rate limit exhausted (key=%q request_id=%q node=%q model=%q)", keyName, requestID, alt.Name, truncateForLog(modelName, 64))
						continue
					}
					metrics.Retry(node.Name)
					lastFailedNode = node.Name
					retryCount++
					nextNode = alt
					nextBody = altBody
					nextDecision = altDecision
					retryErr = e
					return
				}
			}
			// No alternate nodes for the current model. Try the local
			// degradation chain (opt-in) before falling through to
			// cloud - gated on !degradedOnce so a request can only ever
			// substitute once, even across retry-loop iterations (enforces
			// the single-hop invariant in code, not just in a comment, and
			// bounds a cyclic operator config to one substitution instead of
			// looping the retry loop indefinitely).
			if !degradedOnce {
				// Each candidate's node cap is checked as the chain is walked, so a
				// spent cap moves on to the next chain entry (as failover moves to the
				// next node) and a refused candidate leaves no header, metric or
				// model change behind. Still one hop at most: degradedOnce is set on
				// the first committed substitution.
				var altBody []byte
				accept := func(n *router.NodeState, model string) bool {
					b, capOK := profileFor(rewriteModelField(baseBody, model), n, model)
					if !capOK {
						log.Printf("degradation target skipped: model rate limit exhausted (key=%q request_id=%q node=%q model=%q)", keyName, requestID, n.Name, truncateForLog(model, 64))
						return false
					}
					altBody = b
					return true
				}
				if altNode, altModel, _, altDecision, ok := h.tryLocalDegradationChain(modelName, runtimeFilter, allowLocalDegradation, allowedModels, accept); ok {
					baseBody = applyLocalDegradation(rw, baseBody, modelName, altModel)
					modelName = altModel
					lastFailedNode = node.Name
					retryCount++
					nextNode = altNode
					nextBody = altBody
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
						metrics.RequestsTotal(keyName, h.metricModel(modelName), "none", "503")
						return
					}
				}
				// Signal cloud path via sentinel before calling proxyToCloud,
				// which writes the response. The outer loop checks this after
				// serveAndRecoverAbort returns.
				retryErr = errCloudHandled
				rw.Header().Del("X-Marbor-Model-Alias")
				rw.Header().Del("X-Marbor-Model-Fallback")
				cloudBody, cloudModel := cloudRequestFor(baseBody, modelName, clientModelName, aliased)
				h.proxyToCloud(rw, origReq, cloudBody, cloudModel, keyName, requestID, start, clouds, 0)
				return
			}
			// The detail was logged above; return a generic message so upstream
			// topology never leaks to the client.
			writeAPIError(rw, http.StatusBadGateway, "upstream unavailable", "server_error", "upstream_error")
		}

		rec = &statusRecorder{ResponseWriter: w, start: start}
		streamAborted = serveAndRecoverAbort(proxy, rec, r)
		aborted = streamAborted || clientDisconnected

		if retryErr == errCloudHandled {
			// Cloud path handled the response and did its own logging. If the
			// cloud stream died mid-body, surface that to the client too.
			if streamAborted {
				panic(http.ErrAbortHandler)
			}
			return
		}

		if nextNode != nil {
			// Switch to the alternate node and retry. The new node gets its
			// own model profile and rpm/tpm gate, applied to the pristine body.
			node = nextNode
			decision = nextDecision
			acquire(node, modelName)
			body = nextBody
			r.Body = io.NopCloser(bytes.NewReader(body))
			targetURL, err = url.Parse(node.URL)
			if err != nil {
				release()
				writeAPIError(w, http.StatusInternalServerError, "invalid node URL", "server_error", "internal_error")
				return
			}
			warm = false // retried node may not have model warm
			continue
		}

		// Success or terminal failure - exit retry loop. Skip the decrement if
		// ErrorHandler already released this node's slot.
		if !errHandled {
			release()
			success := rec.StatusCode() < 500 && !aborted
			// A client that went away mid-request is not evidence about the
			// node: neither its failure history nor prefix locality is touched.
			// A genuine upstream 5xx is still the node's failure, even if the
			// client also left.
			clientGone := !success && r.Context().Err() != nil && rec.statusCode < 500
			if !clientGone {
				h.router.RecordRequestOutcome(node.Name, success)
			}
			// Rolling prefix locality is recorded ONLY here, on full
			// successful completion of the request that actually finished -
			// never at candidate selection, backend start, response headers,
			// or first token, and never for a timeout/cancellation/error/
			// aborted-stream outcome (all of those either return before this
			// point or leave success false). node is whichever node actually
			// completed the request - the one that served it after any
			// retry/failover, not necessarily the one first selected.
			if !clientGone {
				h.router.RecordPrefixLocality(prefixRecordKey, node.Name, success && modelName == lookupModel)
			}
		}
		break
	}

	// RecordModelUse was only called once, before the retry loop,
	// against the initially selected node/model - on failover or local-
	// degradation substitution to a different node/model, no follow-up call
	// updated LRU/warmth state for the node that actually served the
	// request. Refresh it now if the final node/model differs.
	if node.Name != initialNode || modelName != initialModel {
		h.router.RecordModelUse(node.Name, modelName)
	}

	// Annotate the routing explanation with retry context the router
	// itself never sees - purely a proxy-layer string addition, no change to
	// Reason/Score/Components.
	if decision != nil && retryCount > 0 {
		decision.Detail = fmt.Sprintf("%s (retry attempt %d after node %s failed)", decision.Detail, retryCount+1, lastFailedNode)
	}

	// One elapsed value feeds every sink so the metric, admin log, audit trail
	// and access log can never disagree about how long the request took.
	elapsed := time.Since(start)
	duration := elapsed.Seconds()
	metricStatus := rec.Status()
	if aborted {
		metricStatus = "aborted"
	}
	metrics.RequestsTotal(keyName, h.metricModel(modelName), node.Name, metricStatus)
	metrics.RequestDuration(h.metricModel(modelName), node.Name, duration)
	if ttft := rec.ttft(); ttft > 0 {
		metrics.RequestTTFT(h.metricModel(modelName), node.Name, ttft.Seconds())
		// Feed the real observed TTFT into placement scoring's load-shape
		// weighting - see Router.RecordTTFT and effectiveLoad.
		h.router.RecordTTFT(node.Name, ttft)
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
	latencyMs := int(elapsed.Milliseconds())
	if h.admin != nil {
		tokens := rec.tokenCount(aborted || rec.truncatedTail)
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
		if aliased {
			loggedModel = clientModelName + " -> " + loggedModel
		}
		// rec.statusCode, not StatusCode(): a request cancelled before any
		// response was written has no status, and must not be logged as 200.
		h.admin.LogRequest(requestID, keyName, clientIP, loggedModel, node.Name, status, rec.statusCode, latencyMs, tokens, rec.promptEvalDurationMs(), decision)
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
		auditModel := requestedModelName
		if aliased {
			auditModel = clientModelName + " -> " + requestedModelName
		}
		h.audit.Log(audit.Entry{
			Time:          time.Now(),
			RequestID:     requestID,
			KeyName:       keyName,
			Model:         auditModel,
			Node:          node.Name,
			Status:        auditStatus,
			LatencyMs:     latencyMs,
			Cloud:         false,
			RoutingReason: routingReason,
		})
	}
	accessModel := modelName
	if aliased {
		accessModel = clientModelName + " -> " + modelName
	}
	h.access.Log(AccessLogEntry{
		TimeUnixMs: time.Now().UnixMilli(),
		RequestID:  requestID,
		KeyName:    keyName,
		Model:      accessModel,
		Node:       node.Name,
		Status:     rec.statusCode,
		LatencyMs:  elapsed.Milliseconds(),
		Cloud:      false,
	})

	// Everything is recorded; now let the client see that the stream died. The
	// reverse proxy already wrote a partial body, so swallowing the abort would
	// end a chunked response cleanly and make a truncated reply look complete.
	if streamAborted {
		panic(http.ErrAbortHandler)
	}
}

// cloudRequestFor returns the body and model name a cloud fallback should
// receive. For an aliased request the cloud provider gets the name the
// client originally sent (a cloud provider knows "gpt-4", not the local
// model it was aliased to), even if a fallback or degradation swap changed
// the local model since. A provider's default_model still overrides this
// inside proxyToCloud. Non-aliased requests pass through unchanged.
func cloudRequestFor(body []byte, modelName, clientModelName string, aliased bool) ([]byte, string) {
	// No client model name (the body named none): there is nothing to restore,
	// and writing an empty "model" would corrupt a body that had no such field.
	if clientModelName == "" {
		return body, modelName
	}
	// A fallback-chain substitution changes the local model without any alias,
	// and a cloud provider does not know the local quantization tag either, so
	// the client's name is restored whenever the two differ.
	if !aliased && modelName == clientModelName {
		return body, modelName
	}
	return rewriteModelField(body, clientModelName), clientModelName
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
func (h *Handler) tryLocalDegradationChain(modelName, runtimeFilter string, allowDegradation bool, allowedModels []string, accept func(*router.NodeState, string) bool) (node *router.NodeState, alt string, warm bool, decision *router.RoutingDecision, ok bool) {
	if !allowDegradation {
		return nil, "", false, nil, false
	}
	for _, candidate := range h.router.LocalDegradationChainFor(modelName) {
		if len(allowedModels) > 0 && !slices.Contains(allowedModels, candidate) {
			continue
		}
		if n, w, d := h.router.RouteExcluding(candidate, runtimeFilter, nil); n != nil {
			if accept != nil && !accept(n, candidate) {
				continue
			}
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
			// Authorization is the marbor's own API-key credential; Cookie carries
			// the admin dashboard's httpOnly session cookie if this request came
			// via a browser proxy path. Neither belongs on a backend GPU node.
			if k != "Authorization" && k != "Cookie" {
				req.Header[k] = v
			}
		}
		// Drop hop-by-hop headers (and any header the client names in
		// Connection) before setting marbor's own: ReverseProxy removes the
		// Connection-named headers after this Director runs, so a client
		// sending "Connection: X-Request-ID" could otherwise strip the
		// correlation header marbor sets below.
		stripHopByHop(req.Header)
		// Forward request ID to upstream so marbor and Ollama logs correlate.
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
		"/v1/fine_tuning", // OpenAI's real spelling uses an underscore
		"/v1/files",
		"/v1/uploads",
		"/v1/assistants",
		"/v1/threads",
		"/v1/batches",
		"/v1/vector-stores",
		"/v1/vector_stores", // OpenAI's real spelling uses an underscore
		"/v1/responses",
		"/v1/evals",
	}
	for _, base := range unsupported {
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return path == "/v1/moderations"
}

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
	// FetchModelTags (used underneath) keeps a 30-second cache, so this is
	// cheap on repeated calls.
	seen := h.router.FleetModelStatuses() // name -> status
	// Model aliases are listed so OpenAI-style clients (which fill their model
	// pickers from this endpoint) can see the names they are configured for.
	// An alias row carries its target's real status and is listed only while
	// the target is present. An alias that shadows a real model name replaces
	// that model's row, since every request for the name reaches the target.
	for alias, target := range h.router.ModelAliases() {
		if status, ok := seen[target]; ok {
			seen[alias] = status
		} else {
			delete(seen, alias)
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

	// An alias is checked first: requests for an aliased name always reach
	// its target, so the alias reports the target's status and is not found
	// while the target is absent, even if a real model of that name exists.
	lookup := modelID
	if target, ok := h.router.ResolveModelAlias(modelID); ok {
		lookup = target
	}
	status := h.router.FleetModelStatuses()[lookup]

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
// true when the stream was aborted. The panic is absorbed here only so the
// caller can finish recording; the caller must re-raise http.ErrAbortHandler
// once everything is recorded, otherwise net/http ends the response as a clean
// chunked stream and the client cannot tell a truncated reply from a complete
// one. Any other panic value is re-raised untouched.
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
	// Same guard as the local retry loop's ErrorHandler: don't spend a
	// billable metered cloud API call on a request nobody is waiting for.
	// Checked here (not just at each call site) so it also covers the
	// next-provider recursion inside this function's own ErrorHandler below.
	if r.Context().Err() != nil {
		return
	}
	cloud := &clouds[idx]
	hasNext := idx+1 < len(clouds)
	delegated := false
	metrics.CloudFallback(cloud.Name)
	path := translateCloudPath(r.URL.Path)

	// Anthropic has no embeddings endpoint. Chat/completions requests are
	// translated to Anthropic's native /v1/messages schema by
	// anthropicTransport below; embeddings has no equivalent to translate to,
	// so return a clear 501 before the request leaves the marbor.
	if strings.EqualFold(cloud.Provider, "anthropic") && path == "/v1/embeddings" {
		if hasNext {
			h.proxyToCloud(w, r, body, modelName, keyName, requestID, start, clouds, idx+1)
			return
		}
		writeAPIError(w, http.StatusNotImplemented,
			"the Anthropic cloud provider does not support "+path+" through marbor; use an OpenAI-compatible overflow provider for this endpoint",
			"invalid_request_error", "unsupported_cloud_endpoint")
		metrics.RequestsTotal(keyName, h.metricModel(modelName), "cloud:"+cloud.Name, "501")
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
	// default_model is operator configuration (set by an admin, never by a
	// client): when set it deliberately overrides whatever model the client
	// named, so a provider that serves one model is always sent that model.
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
	// header wait is bounded - the streaming body is untouched, so streaming
	// stays streaming.
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
		// The client's query string is not forwarded: a third-party provider has no
		// use for it, and it can carry credentials meant for marbor.
		req.URL.RawQuery = ""
		req.Host = targetURL.Host
		// Only an allow-list of client headers is forwarded to a third-party
		// provider. Everything else a client can send (cookies, client-address
		// and proxy headers, session ids, other credentials, org/project
		// selectors, marbor's own headers) stays behind. Accept-Encoding is
		// also withheld so the transport negotiates (and decodes) its own.
		// Content-Length is set from the rewritten body below.
		out := make(http.Header)
		for _, name := range cloudForwardedHeaders {
			if v, ok := r.Header[name]; ok {
				out[name] = slices.Clone(v)
			}
		}
		req.Header = out
		stripHopByHop(req.Header)
		req.Header.Set("User-Agent", "marbor")
		if requestID != "" {
			req.Header.Set("X-Request-ID", requestID)
		}
		// A nil value stops ReverseProxy appending the client IP to
		// X-Forwarded-For.
		req.Header["X-Forwarded-For"] = nil
		req.Header.Set("Authorization", "Bearer "+cloud.APIKey)
		if len(outBody) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(outBody))
			req.ContentLength = int64(len(outBody))
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, outReq *http.Request, err error) {
		// Log the detailed error server-side, but never leak upstream topology
		// (hostnames, ports, dial/TLS details) to the client.
		log.Printf("cloud upstream error (provider=%q request_id=%q): %v", cloud.Name, requestID, err)
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
		// The next provider already recorded the request; if its stream died
		// mid-body, keep surfacing that to the client.
		if aborted {
			panic(http.ErrAbortHandler)
		}
		return
	}

	elapsed := time.Since(start)
	duration := elapsed.Seconds()
	nodeName := "cloud:" + cloud.Name
	status := "cloud"
	metricStatus := rec.Status()
	if aborted {
		status = "aborted"
		metricStatus = "aborted"
	}
	metrics.RequestsTotal(keyName, h.metricModel(modelName), nodeName, metricStatus)
	metrics.RequestDuration(h.metricModel(modelName), nodeName, duration)
	if ttft := rec.ttft(); ttft > 0 {
		metrics.RequestTTFT(h.metricModel(modelName), nodeName, ttft.Seconds())
	}

	if h.admin != nil {
		latencyMs := int(elapsed.Milliseconds())
		tokens := rec.tokenCount(aborted || rec.truncatedTail)
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
		h.admin.LogRequest(requestID, keyName, clientIP, loggedModel, nodeName, status, rec.statusCode, latencyMs, tokens, 0, nil)
		if tokens >= 0 {
			h.admin.TrackCloudCostModel(keyName, cloud.Name, modelName, cloud.CostPer1KTokens, tokens)
			// Model-config rpm/tpm caps are keyed to a specific local marbor
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
			LatencyMs:  int(elapsed.Milliseconds()),
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
		LatencyMs:  elapsed.Milliseconds(),
		Cloud:      true,
	})

	// Recording is done; let the client see that the cloud stream died.
	if aborted {
		panic(http.ErrAbortHandler)
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
	case "/api/embed":
		return "/v1/embeddings"
	default:
		return ollamaPath
	}
}

// truncateForLog bounds a client-supplied string for a log line.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// cloudForwardedHeaders is the allow-list of client request headers a cloud
// provider may receive. Credentials and the provider-specific headers
// (Authorization, x-api-key, anthropic-version) are set by marbor itself.
var cloudForwardedHeaders = []string{"Content-Type", "Accept"}

// stripHopByHop removes the hop-by-hop headers from h, plus every header named
// in a Connection header.
func stripHopByHop(h http.Header) {
	for _, v := range h["Connection"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(name)
	}
}

// canonicalPath returns p with "." and ".." segments and duplicate slashes
// resolved, keeping a trailing slash if p had one. An empty path is returned
// unchanged.
func canonicalPath(p string) string {
	if p == "" {
		return p
	}
	cleaned := path.Clean(p)
	if strings.HasSuffix(p, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}

// maxBodyJSONDepth bounds how deeply nested a request body may be before it is
// rejected. Real requests nest a handful of levels; a body that nests
// thousands deep is an attack on whatever parses it next.
const maxBodyJSONDepth = 1000

// skipJSONValue consumes one JSON value from dec token by token without
// building a decoded copy of it, so a large value (an image, a long prompt)
// is not materialised just to be discarded. It is iterative, with an explicit
// depth counter, so nesting cannot exhaust the stack. Token() still allocates
// a transient string for each string token it reads; those are garbage at
// once. Returns false on a syntax error or when nesting passes
// maxBodyJSONDepth.
func skipJSONValue(dec *json.Decoder) bool {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
				if depth > maxBodyJSONDepth {
					return false
				}
			} else {
				depth--
			}
		}
		if depth == 0 {
			return true
		}
	}
}

// modelFieldProblem inspects a request body that starts as a JSON object and
// returns a short reason if it must be rejected, or "" if it is fine. A body
// is rejected when its "model" field is ambiguous (the exact key repeated, or
// another key equal to "model" ignoring case, which Go's decoder merges while
// case-sensitive backends do not), or when the object is malformed, has
// trailing data, or nests too deeply for the check to read safely. Bodies
// that are not a JSON object (empty, an array, plain text) are never flagged
// here: they carry no model for anyone to misread.
func modelFieldProblem(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	exact := 0
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "invalid JSON"
		}
		key, ok := tok.(string)
		if !ok {
			return "invalid JSON"
		}
		if key == "model" {
			exact++
			if exact > 1 {
				return "ambiguous model field"
			}
		} else if strings.EqualFold(key, "model") {
			return "ambiguous model field"
		}
		if !skipJSONValue(dec) {
			return "invalid or too deeply nested JSON"
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return "invalid JSON"
	}
	if _, err := dec.Token(); err != io.EOF {
		return "invalid JSON"
	}
	return ""
}

// hasAmbiguousModelKey reports whether modelFieldProblem finds anything wrong
// with the body.
func hasAmbiguousModelKey(body []byte) bool {
	return modelFieldProblem(body) != ""
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
	// Drop case variants ("Model", "MODEL") so a case-sensitive backend cannot
	// read a different name than the one written here.
	for k := range m {
		if k != "model" && strings.EqualFold(k, "model") {
			delete(m, k)
		}
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
	statusCode    int
	tail          []byte    // retained body for token-count parsing - see tailMax
	sawNewline    bool      // true once a '\n' has appeared in the written body
	truncatedTail bool      // true if the no-newline tail hit embedTailMax before the response finished
	heldLine      bool      // true while the tail is one retained line longer than tailMax (see trimTail)
	start         time.Time // request start, for TTFT; zero value means TTFT is unavailable
	firstByteAt   time.Time // set on the first Write(); zero until then
}

// tailMax bounds the retained response tail for line-oriented responses -
// Ollama NDJSON (one JSON object per line) or OpenAI SSE ("data: " lines) -
// where the token count lives in the final line, so a small tail is enough.
//
// A single-JSON-document response (e.g. /v1/embeddings, whose "usage" field
// trails a large embedding array with no newline anywhere in the body) can't
// be identified by "final line" at all, so Write does not truncate until it
// has seen at least one '\n' - see sawNewline. Until then the buffer grows up
// to embedTailMax. Writes still pass straight through in both cases -
// streaming to the client is never buffered.
const tailMax = 8192

// embedTailMax bounds the no-newline retention path (see tailMax doc above).
// It used to grow all the way to maxRequestBodyBytes (32 MiB) per request -
// a burst of concurrent /v1/embeddings requests could each hold a 32 MiB
// tail, OOM-killing the control plane under anonymous traffic.
// 1 MiB is generous headroom for the "model"/"usage" fields that surround a
// real embedding array while keeping worst-case per-request retention two
// orders of magnitude below the old bound. A body that exceeds this before
// finishing is marked truncatedTail so tokenCount reports -1 (unknown)
// rather than a fake 0 for a body it never fully saw.
const embedTailMax = 1 << 20 // 1 MiB

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
			if len(r.tail) < embedTailMax {
				r.tail = append(r.tail, b[:n]...)
			} else {
				r.truncatedTail = true
			}
		} else if !r.truncatedTail {
			// Once the tail is known to be truncated the count is unknown whatever
			// follows, so retention stops: the response keeps streaming to the client
			// untouched, but no more bytes are copied or rescanned.
			r.tail = append(r.tail, b[:n]...)
			if len(r.tail) > tailMax {
				if r.heldLine && bytes.IndexByte(b[:n], '\n') < 0 {
					// The tail is a single protected line that is still growing:
					// nothing new to trim or rescan until a newline arrives, only the
					// embedTailMax bound to enforce. This keeps a long line written in
					// many small chunks linear instead of rescanning it on every write.
					if len(r.tail) > embedTailMax {
						r.tail = nil
						r.truncatedTail = true
						r.heldLine = false
					}
				} else {
					r.trimTail()
				}
			}
		}
	}
	return n, err
}

// trimTail shrinks an over-long line-oriented tail to about tailMax, always on
// a line boundary and never dropping the final line: that line carries the
// token count, so cutting it (or cutting inside it) would turn a real count
// into a fabricated zero. If the final line alone exceeds tailMax it is kept
// whole up to embedTailMax; beyond that the tail is discarded and
// truncatedTail is set, so the caller reports the count as unknown.
func (r *statusRecorder) trimTail() {
	tail := r.tail
	if len(tail) <= tailMax {
		return
	}
	// Never cut at or past the last line tokenCount would read.
	protect := lastJSONLineStart(tail)

	start := len(tail) - tailMax
	if start > 0 && tail[start-1] != '\n' {
		if idx := bytes.IndexByte(tail[start:], '\n'); idx >= 0 {
			start += idx + 1
		}
	}
	if start > protect {
		start = protect
	}
	if len(tail)-start > embedTailMax {
		r.tail = nil
		r.truncatedTail = true
		r.heldLine = false
		return
	}
	if start == 0 {
		r.heldLine = len(tail) > tailMax
		return
	}
	// Shift in place: no allocation or second copy of the buffer.
	n := copy(tail, tail[start:])
	r.tail = tail[:n]
	r.heldLine = n > tailMax
}

// lastJSONLineStart returns the offset of the last line in tail that holds a
// JSON object (bare NDJSON, or SSE "data: {...}"), or len(tail) if none does.
// SSE streams end with "data: [DONE]" after the usage line, so the final line
// is not always the one that carries the token count.
func lastJSONLineStart(tail []byte) int {
	end := len(tail)
	for end > 0 {
		s := bytes.LastIndexByte(tail[:end], '\n') + 1
		line := bytes.TrimPrefix(bytes.TrimSpace(tail[s:end]), []byte("data: "))
		if len(line) > 0 && line[0] == '{' {
			return s
		}
		if s == 0 {
			break
		}
		end = s - 1
	}
	return len(tail)
}

// usageDoc is the part of a response document that carries a token count.
type usageDoc struct {
	EvalCount       int64           `json:"eval_count"`
	PromptEvalCount int64           `json:"prompt_eval_count"`
	Embedding       json.RawMessage `json:"embedding"`
	Usage           struct {
		TotalTokens int64 `json:"total_tokens"`
	} `json:"usage"`
}

// count returns the token count the document reports. final is false when it
// carries none, so the caller keeps looking; a legacy embeddings reply reports
// -1 (genuinely no count), never a fake zero.
func (u usageDoc) count() (n int64, final bool) {
	if k := u.EvalCount + u.PromptEvalCount; k > 0 {
		return k, true
	}
	if u.Usage.TotalTokens > 0 {
		return u.Usage.TotalTokens, true
	}
	if u.Embedding != nil {
		return -1, true
	}
	return 0, false
}

// wholeTailJSON decodes the entire retained tail as one JSON object into v.
// It reports false unless the tail is exactly one object, so NDJSON, SSE and a
// cut-off tail are left to line-wise parsing.
func (r *statusRecorder) wholeTailJSON(v any) bool {
	doc := bytes.TrimSpace(r.tail)
	if len(doc) == 0 || doc[0] != '{' {
		return false
	}
	return json.Unmarshal(doc, v) == nil
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
// endpoint genuinely reports no token count, so 0 would be a fake zero.
func (r *statusRecorder) tokenCount(aborted bool) int64 {
	// A pretty-printed (multi-line) JSON reply is one document, not lines, so
	// the whole tail is tried as a single value first; line-wise parsing below
	// is for NDJSON and SSE.
	var whole usageDoc
	if r.wholeTailJSON(&whole) {
		if n, final := whole.count(); final {
			return n
		}
		if aborted {
			return -1
		}
		return 0
	}
	lines := bytes.Split(r.tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		line = bytes.TrimPrefix(line, []byte("data: ")) // SSE framing
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var t usageDoc
		if err := json.Unmarshal(line, &t); err != nil {
			// The last JSON-looking line is unreadable (cut off mid-object, or
			// malformed). An earlier line's count would describe a different
			// point in the stream, so the honest answer is unknown, not a
			// silent fall back to it.
			return -1
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
		// zero-token measurement - never present a fake zero as real.
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
	var whole struct {
		EvalDuration int64 `json:"eval_duration"`
	}
	if r.wholeTailJSON(&whole) {
		return whole.EvalDuration / int64(time.Millisecond)
	}
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

// promptEvalDurationMs parses Ollama's real prompt_eval_duration (nanoseconds
// spent processing the prompt, excluding generation) from the response tail
// - the prefill half of evalDurationMs's generation half. Only present on
// Ollama-native responses; returns 0 (unavailable) for anything else,
// including OpenAI usage blocks and requests with no prompt to process
// (Ollama omits the field entirely rather than reporting a real zero in
// that case). 0 always means "not present", same convention as
// evalDurationMs.
func (r *statusRecorder) promptEvalDurationMs() int64 {
	var whole struct {
		PromptEvalDuration int64 `json:"prompt_eval_duration"`
	}
	if r.wholeTailJSON(&whole) {
		return whole.PromptEvalDuration / int64(time.Millisecond)
	}
	lines := bytes.Split(r.tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		line = bytes.TrimPrefix(line, []byte("data: ")) // SSE framing
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var t struct {
			PromptEvalDuration int64 `json:"prompt_eval_duration"`
		}
		if err := json.Unmarshal(line, &t); err != nil {
			continue
		}
		if t.PromptEvalDuration > 0 {
			return t.PromptEvalDuration / int64(time.Millisecond)
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

// metricModel returns the model label to use in Prometheus series for name.
// A model the fleet actually knows (resident on a node, an operator alias, or
// one with a configured fallback or degradation chain) keeps its name; any
// other name is client-chosen and shares the single label "unknown", so a
// caller cannot mint metric series by inventing model names. The check reads
// only in-memory state, never the network.
func (h *Handler) metricModel(name string) string {
	if name == "" {
		return ""
	}
	if _, ok := h.router.ResolveModelAlias(name); ok {
		return name
	}
	if len(h.router.FallbackChainFor(name)) > 0 || len(h.router.LocalDegradationChainFor(name)) > 0 {
		return name
	}
	for _, n := range h.router.Nodes() {
		n.RLock()
		for _, m := range n.LoadedModels {
			if m.Name == name {
				n.RUnlock()
				return name
			}
		}
		n.RUnlock()
	}
	return "unknown"
}
