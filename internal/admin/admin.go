package admin

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/ha"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

//go:embed web/dist
var webFS embed.FS

type Server struct {
	router        *router.Router
	auth          *auth.Middleware
	cfg           config.Config
	adminToken    string
	version       string
	configPath    string
	mu            sync.RWMutex
	requests      []RequestLog
	localCount    int64   // atomic - requests served by local nodes
	cloudCount    int64   // atomic - requests forwarded to cloud
	localTokens   int64   // atomic - real token counts parsed from local node responses
	cloudTokens   int64   // atomic - real token counts parsed from cloud responses
	cloudSpentUSD float64 // protected by mu
	refCostPer1K  float64 // reference cloud rate used to value local tokens (immutable after construction)
	startTime     time.Time
	analytics     *analyticsStore
	auditLog      *audit.Logger
	haMonitor     *ha.Monitor // nil when HA disabled
}

// SetVersion sets the version string reported by /health.
// Call this from main with the ldflags-injected version before serving.
func (s *Server) SetVersion(v string) {
	s.version = v
}

// SetConfigPath sets the path used by handleUpdateSettings to persist config.
func (s *Server) SetConfigPath(p string) {
	s.configPath = p
}

// SetAuditLogger wires the audit logger into the admin server for query access.
func (s *Server) SetAuditLogger(al *audit.Logger) {
	s.auditLog = al
}

// SetHAMonitor wires the HA monitor into the admin server.
func (s *Server) SetHAMonitor(m *ha.Monitor) { s.haMonitor = m }

type RequestLog struct {
	ID           string    `json:"id"`
	ApiKey       string    `json:"apiKey"`
	Model        string    `json:"model"`
	Node         string    `json:"routedTo"`
	Status       string    `json:"status"`
	Latency      int       `json:"latency"`
	Tokens       int64     `json:"tokens"`
	TokensPerSec float64   `json:"tokensPerSec"`
	Time         time.Time `json:"time"`
}

type nodeResp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Port        int    `json:"port"`
	GPUModel    string `json:"gpuModel"`
	VRAMTotalMB int64  `json:"vramTotalMB"`
	VRAMUsedMB  int64  `json:"vramUsedMB"`
	// VRAMSource: "nvidia" (live local nvidia-smi), "api" (summed from the node's
	// own /api/ps size_vram, total unknown), "declared" (total from config), "none".
	VRAMSource    string             `json:"vramSource"`
	PowerDrawW    float64            `json:"powerDrawW"`
	Temperature   *float64           `json:"temperature"`
	Health        string             `json:"health"`
	Draining      bool               `json:"draining"`
	Uptime        string             `json:"uptime"`
	LoadedModels  []router.ModelInfo `json:"loadedModels"`
	ActiveConns   int32              `json:"activeConns"`
	HealthHistory []float64          `json:"healthHistory"`
}

type keyResp struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Key               string   `json:"key"`
	Created           string   `json:"created"`
	RequestsToday     int      `json:"requestsToday"`
	RequestsThisMonth int      `json:"requestsThisMonth"`
	TokensThisMonth   int64    `json:"tokensThisMonth"`
	EstimatedCostUsd  float64  `json:"estimatedCostUsd"`
	RateLimit         int      `json:"rateLimit"`
	Status            string   `json:"status"`
	AllowedModels     []string `json:"allowedModels"`
	ExpiresAt         string   `json:"expiresAt,omitempty"`
}

func NewServer(r *router.Router, a *auth.Middleware, cfg config.Config) *Server {
	// Precedence: explicit AdminToken wins; then the first auth key when auth is
	// enabled; otherwise generate a cryptographically-random token. The admin API
	// binds all interfaces by default, so a constant fallback (e.g. "admin") is a
	// LAN-takeover footgun — never fall back to a guessable literal.
	var token string
	switch {
	case cfg.Auth.AdminToken != "":
		token = cfg.Auth.AdminToken
	case cfg.Auth.Enabled && len(cfg.Auth.Keys) > 0:
		token = cfg.Auth.Keys[0].Key
	default:
		token = generateAdminToken()
		log.Printf("admin: no admin_token configured; generated a random one for this run: %s", token)
	}
	// Mirror config.Validate()'s default so servers constructed from a zero
	// config (tests, embedded use) still value local tokens at a real rate.
	refRate := cfg.Savings.ReferenceCostPer1K
	if refRate <= 0 {
		refRate = 0.002
	}
	return &Server{
		router:       r,
		auth:         a,
		cfg:          cfg,
		adminToken:   token,
		version:      "dev",
		configPath:   "config.yaml",
		refCostPer1K: refRate,
		startTime:    time.Now(),
		analytics:    newAnalyticsStore(refRate),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// reg registers a route on both /admin/X and /admin/v1/X for backward compat.
	reg := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)
		// "GET /admin/foo" -> "GET /admin/v1/foo"
		v1 := strings.Replace(pattern, "/admin/", "/admin/v1/", 1)
		mux.HandleFunc(v1, h)
	}

	reg("GET /admin/nodes", s.cors(s.adminAuth(s.handleNodes)))
	reg("GET /admin/nodes/{name}", s.cors(s.adminAuth(s.handleNode)))
	reg("POST /admin/nodes", s.cors(s.adminAuth(s.handleAddNode)))
	reg("DELETE /admin/nodes/{name}", s.cors(s.adminAuth(s.handleRemoveNode)))

	reg("GET /admin/keys", s.cors(s.adminAuth(s.handleKeys)))
	reg("POST /admin/keys", s.cors(s.adminAuth(s.handleAddKey)))
	reg("DELETE /admin/keys/{name}", s.cors(s.adminAuth(s.handleRevokeKey)))

	reg("GET /admin/routing/rules", s.cors(s.adminAuth(s.handleRoutingRules)))
	reg("POST /admin/routing/rules", s.cors(s.adminAuth(s.handleAddRoutingRule)))
	reg("DELETE /admin/routing/rules/{id}", s.cors(s.adminAuth(s.handleRemoveRoutingRule)))
	reg("PUT /admin/routing/rules/{id}/toggle", s.cors(s.adminAuth(s.handleToggleRoutingRule)))
	reg("GET /admin/routing/strategy", s.cors(s.adminAuth(s.handleGetRoutingStrategy)))
	reg("PUT /admin/routing/strategy", s.cors(s.adminAuth(s.handleSetRoutingStrategy)))

	reg("GET /admin/settings", s.cors(s.adminAuth(s.handleSettings)))
	reg("PUT /admin/settings", s.cors(s.adminAuth(s.handleUpdateSettings)))

	reg("GET /admin/requests/live", s.cors(s.adminAuth(s.handleLiveRequests)))
	reg("GET /admin/metrics/summary", s.cors(s.adminAuth(s.handleSummary)))
	reg("GET /admin/metrics/savings", s.cors(s.adminAuth(s.handleSavings)))
	reg("GET /admin/cloud/providers", s.cors(s.adminAuth(s.handleCloudProviders)))
	reg("GET /admin/analytics", s.cors(s.adminAuth(s.handleAnalytics)))
	reg("GET /admin/analytics/export", s.cors(s.adminAuth(s.handleAnalyticsExport)))
	reg("GET /admin/models", s.cors(s.adminAuth(s.handleModels)))
	reg("POST /admin/nodes/{name}/pull", s.cors(s.adminAuth(s.handleNodePull)))
	reg("POST /admin/nodes/{name}/drain", s.cors(s.adminAuth(s.handleDrainNode)))
	reg("DELETE /admin/nodes/{name}/drain", s.cors(s.adminAuth(s.handleUndrainNode)))
	reg("GET /admin/audit", s.cors(s.adminAuth(s.handleAudit)))
	reg("GET /admin/nodes/model-fit", s.cors(s.adminAuth(s.handleModelFit)))
	reg("GET /admin/models/catalog", s.cors(s.adminAuth(s.handleModelCatalog)))

	reg("GET /admin/ha/peers", s.cors(s.adminAuth(s.handleHAPeers)))

	reg("GET /admin/warmup", s.cors(s.adminAuth(s.handleWarmupStatus)))
	reg("POST /admin/warmup/ping", s.cors(s.adminAuth(s.handleWarmupPing)))

	reg("POST /admin/config/reload", s.cors(s.adminAuth(s.handleConfigReload)))
	reg("GET /admin/config", s.cors(s.adminAuth(s.handleGetConfig)))

	// Health check — no auth required. Used by load balancers and Docker healthchecks.
	mux.HandleFunc("GET /health", s.handleHealth)

	if sub, err := fs.Sub(webFS, "web/dist"); err == nil {
		mux.Handle("/assets/", s.noCache(http.FileServer(http.FS(sub))))
	} else {
		fmt.Println("warn: failed to embed web UI:", err)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve index.html for all non-API routes (SPA fallback)
		if strings.HasPrefix(r.URL.Path, "/admin") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		sub, _ := fs.Sub(webFS, "web/dist")
		http.ServeFileFS(w, r, sub, "index.html")
	})

	return mux
}

func (s *Server) noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		origin := s.cfg.Admin.CORSOrigin
		s.mu.RUnlock()
		// Default (empty origin): emit no CORS headers so the mutating admin API
		// is only callable same-origin (the embedded dashboard). A wildcard on an
		// authed, mutating API is a CSRF/exfil footgun, so it is opt-in only.
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		// Constant-time compare so a timing side channel cannot reveal the
		// admin token byte-by-byte. Length is part of the comparison.
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next(w, r)
	}
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	nodes := s.router.Nodes()
	out := make([]nodeResp, len(nodes))
	for i, n := range nodes {
		n.RLock()
		port := 0
		if u, err := url.Parse(n.URL); err == nil {
			port, _ = strconv.Atoi(u.Port())
		}
		health := "healthy"
		if !n.Healthy {
			health = "down"
		} else if n.Failures > 0 {
			health = "degraded"
		}
		// Empty history stays empty ([] in JSON) — the UI renders a "no data" state.
		hist := make([]float64, len(n.HealthHistory))
		copy(hist, n.HealthHistory)
		out[i] = nodeResp{
			ID:            fmt.Sprintf("gpu-%d", i),
			Name:          n.Name,
			Port:          port,
			GPUModel:      n.GPUModel,
			VRAMTotalMB:   n.VRAMTotalMB,
			VRAMUsedMB:    n.VRAMUsedMB,
			VRAMSource:    n.VRAMSource,
			PowerDrawW:    n.PowerDrawW,
			Temperature:   n.Temperature,
			Health:        health,
			Draining:      n.Draining,
			Uptime:        n.Uptime,
			LoadedModels:  append([]router.ModelInfo(nil), n.LoadedModels...),
			ActiveConns:   atomic.LoadInt32(&n.ActiveConns),
			HealthHistory: hist,
		}
		n.RUnlock()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleNode returns a single node by name.
// GET /admin/nodes/{name} (also /admin/v1/nodes/{name})
func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	nodes := s.router.Nodes()
	for i, n := range nodes {
		if n.Name != name {
			continue
		}
		n.RLock()
		port := 0
		if u, err := url.Parse(n.URL); err == nil {
			port, _ = strconv.Atoi(u.Port())
		}
		health := "healthy"
		if !n.Healthy {
			health = "down"
		} else if n.Failures > 0 {
			health = "degraded"
		}
		hist := make([]float64, len(n.HealthHistory))
		copy(hist, n.HealthHistory)
		out := nodeResp{
			ID:            fmt.Sprintf("gpu-%d", i),
			Name:          n.Name,
			Port:          port,
			GPUModel:      n.GPUModel,
			VRAMTotalMB:   n.VRAMTotalMB,
			VRAMUsedMB:    n.VRAMUsedMB,
			VRAMSource:    n.VRAMSource,
			PowerDrawW:    n.PowerDrawW,
			Temperature:   n.Temperature,
			Health:        health,
			Draining:      n.Draining,
			Uptime:        n.Uptime,
			LoadedModels:  append([]router.ModelInfo(nil), n.LoadedModels...),
			ActiveConns:   atomic.LoadInt32(&n.ActiveConns),
			HealthHistory: hist,
		}
		n.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	http.Error(w, fmt.Sprintf(`{"error":"node %q not found"}`, name), http.StatusNotFound)
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	// Snapshot the keys slice under lock; handleUpdateSettings replaces s.cfg
	// concurrently. The backing array is never mutated in place, so a header
	// copy is sufficient.
	s.mu.RLock()
	keys := s.cfg.Auth.Keys
	s.mu.RUnlock()
	out := make([]keyResp, 0, len(keys))
	for i, k := range keys {
		today, month, models, expires, rateLimit, createdAt, ok := 0, 0, []string(nil), "", k.RateLimit, time.Time{}, false
		var tokensMonth int64
		if s.auth != nil {
			today, month, tokensMonth, models, expires, rateLimit, createdAt, ok = s.auth.KeyStats(k.Name)
		}
		// Estimated cost values this month's tokens at the configured reference
		// rate. It is an estimate, not a billed amount.
		estimatedCost := float64(tokensMonth) / 1000.0 * s.refCostPer1K
		if !ok {
			models = k.Models
			expires = k.ExpiresAt
			rateLimit = k.RateLimit
		}

		// Determine status: revoked if not present in auth, expired if past expiresAt, else active.
		status := "active"
		if s.auth != nil && !ok {
			status = "revoked"
		} else if expires != "" {
			// Match auth.keyExpired: accept a date (valid through end of day) or
			// RFC3339, so date-only expiries are labelled correctly (auth already
			// rejects them; this keeps the UI status in sync).
			now := time.Now()
			if t, err := time.Parse("2006-01-02", expires); err == nil {
				if now.After(t.Add(24 * time.Hour)) {
					status = "expired"
				}
			} else if t, err := time.Parse(time.RFC3339, expires); err == nil && now.After(t) {
				status = "expired"
			}
		}

		created := ""
		if !createdAt.IsZero() {
			created = createdAt.Format(time.RFC3339)
		}
		out = append(out, keyResp{
			ID:   fmt.Sprintf("key-%d", i+1),
			Name: k.Name,
			// Never re-serve the full secret. The plaintext key is shown once at
			// creation (handleAddKey); the list only carries a masked preview.
			Key:               maskKey(k.Key),
			Created:           created,
			RequestsToday:     today,
			RequestsThisMonth: month,
			TokensThisMonth:   tokensMonth,
			EstimatedCostUsd:  estimatedCost,
			RateLimit:         rateLimit,
			Status:            status,
			AllowedModels:     models,
			ExpiresAt:         expires,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleLiveRequests(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	reqs := make([]RequestLog, len(s.requests))
	copy(reqs, s.requests)
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reqs)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	nodes := s.router.Nodes()
	totalConns := int32(0)
	online := 0
	for _, n := range nodes {
		n.RLock()
		healthy := n.Healthy
		n.RUnlock()
		if healthy {
			online++
		}
		totalConns += atomic.LoadInt32(&n.ActiveConns)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active_requests": totalConns,
		"nodes_online":    online,
		"total_nodes":     len(nodes),
		"queue_depth":     s.router.QueueDepth(),
	})
}

func (s *Server) handleWarmupStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.cfg.Warmup
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

func (s *Server) handleWarmupPing(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	enabled := s.cfg.Warmup.Enabled
	s.mu.RUnlock()
	if !enabled {
		http.Error(w, `{"error":"warmup not enabled"}`, http.StatusBadRequest)
		return
	}
	s.router.TriggerWarmup(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"triggered"}`))
}

// handleConfigReload reloads the config file from disk without restarting.
// POST /admin/config/reload (also /admin/v1/config/reload)
// Equivalent to sending SIGHUP — useful in container environments where
// sending Unix signals is inconvenient (Kubernetes, Nomad, etc.).
func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	newCfg, err := config.LoadConfig(s.configPath)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	s.auth.Reload(newCfg.Auth)
	s.router.SetWarmupConfig(newCfg.Warmup)
	s.mu.Lock()
	s.cfg = *newCfg
	s.mu.Unlock()
	log.Printf("config reloaded via API from %s (auth keys: %d, warmup: %v)", s.configPath, len(newCfg.Auth.Keys), newCfg.Warmup.Enabled)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"reloaded":true,"config_path":%q,"auth_keys":%d,"warmup_enabled":%v}`,
		s.configPath, len(newCfg.Auth.Keys), newCfg.Warmup.Enabled)
}

// handleGetConfig returns the current running config with all secret values masked.
// GET /admin/config (also /admin/v1/config)
// AdminToken is already json:"-"; this handler additionally masks API key values
// and cloud provider keys so the response is safe to log or share for debugging.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	// Deep-copy and mask secrets before serialising.
	masked := cfg
	keys := make([]config.KeyConfig, len(cfg.Auth.Keys))
	copy(keys, cfg.Auth.Keys)
	for i := range keys {
		keys[i].Key = "***"
	}
	masked.Auth.Keys = keys

	providers := make([]config.CloudProvider, len(cfg.CloudProviders))
	copy(providers, cfg.CloudProviders)
	for i := range providers {
		providers[i].APIKey = "***"
	}
	masked.CloudProviders = providers

	if masked.Webhook.Secret != "" {
		masked.Webhook.Secret = "***"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(masked)
}

func (s *Server) handleAddNode(w http.ResponseWriter, r *http.Request) {
	var cfg config.NodeConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.router.AddNode(cfg)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleRemoveNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.router.RemoveNode(name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDrainNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.router.DrainNode(name) {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"node %q not found"}`, name), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"node":%q,"draining":true}`, name)
}

func (s *Server) handleUndrainNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.router.UndrainNode(name) {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"node %q not found"}`, name), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"node":%q,"draining":false}`, name)
}

// generateAPIKey creates a cryptographically random API key of the form sk-<name>-<48 hex chars>.
func generateAPIKey(name string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "sk-" + name + "-" + hex.EncodeToString(b)
}

// generateAdminToken returns a cryptographically-random 64-char hex token
// (32 bytes). A failing CSPRNG at startup is unrecoverable for security, so we
// fail closed via log.Fatalf rather than serve a weak/empty token.
func generateAdminToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("admin: failed to generate admin token from crypto/rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// AdminToken returns the bearer token the admin API authenticates against.
// Exposed for tests and callers that need to authenticate against a server
// whose token was auto-generated; harmless to expose since the token must be
// presented as a bearer credential anyway.
func (s *Server) AdminToken() string { return s.adminToken }

// maskKey returns a non-reversible preview of an API key so the list endpoint
// never re-serves the full secret. Format: first 7 chars + "…" + last 4
// (e.g. "sk-prod…a1b2"). Returns "" when the key is too short to mask safely.
func maskKey(k string) string {
	if len(k) < 12 {
		return ""
	}
	return k[:7] + "…" + k[len(k)-4:]
}

func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
	var k config.KeyConfig
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if k.Key == "" {
		k.Key = generateAPIKey(k.Name)
	}
	if s.auth != nil {
		s.auth.AddKey(k)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(k)
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.auth != nil {
		s.auth.RevokeKey(name)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRoutingRules(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.router.Rules())
}

func (s *Server) handleAddRoutingRule(w http.ResponseWriter, r *http.Request) {
	var rule config.RoutingRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.router.AddRule(rule)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleRemoveRoutingRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.router.RemoveRule(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleToggleRoutingRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.router.ToggleRule(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRoutingStrategy(w http.ResponseWriter, r *http.Request) {
	strategy := s.router.Strategy()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"strategy": strategy})
}

func (s *Server) handleSetRoutingStrategy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Strategy string `json:"strategy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.router.SetStrategy(req.Strategy)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	// Mask sensitive fields before returning to the client.
	for i := range cfg.CloudProviders {
		if cfg.CloudProviders[i].APIKey != "" {
			cfg.CloudProviders[i].APIKey = "***"
		}
	}
	cfg.Auth.AdminToken = ""
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var incoming config.Config
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	// Preserve fields the UI doesn't send to avoid silently zeroing them.
	if len(incoming.CloudProviders) == 0 {
		incoming.CloudProviders = s.cfg.CloudProviders
	}
	if len(incoming.Auth.Keys) == 0 {
		incoming.Auth.Keys = s.cfg.Auth.Keys
	}
	incoming.Auth.AdminToken = s.cfg.Auth.AdminToken
	s.cfg = incoming
	cfg := s.cfg
	s.mu.Unlock()
	if err := config.SaveConfig(s.configPath, cfg); err != nil {
		http.Error(w, fmt.Sprintf("failed to save config: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) LogRequest(apiKey, model, node, status string, latencyMs int, tokens int64) {
	var tps float64
	if tokens > 0 && latencyMs > 0 {
		tps = float64(tokens) / (float64(latencyMs) / 1000.0)
	}
	// Attribute token usage to the calling key for per-key analytics + cost.
	if s.auth != nil {
		s.auth.AddKeyTokens(apiKey, tokens)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, RequestLog{
		ID:           fmt.Sprintf("req-%d", time.Now().UnixNano()),
		ApiKey:       apiKey,
		Model:        model,
		Node:         node,
		Status:       status,
		Latency:      latencyMs,
		Tokens:       tokens,
		TokensPerSec: tps,
		Time:         time.Now(),
	})
	if len(s.requests) > 50 {
		s.requests = s.requests[len(s.requests)-50:]
	}
}

// TrackLocalRequestModel tracks a local request with model-level granularity.
// tokens is the real token count parsed from the response (eval_count +
// prompt_eval_count); 0 means the count was unavailable and contributes
// nothing to savings.
func (s *Server) TrackLocalRequestModel(model string, tokens int64) {
	atomic.AddInt64(&s.localCount, 1)
	atomic.AddInt64(&s.localTokens, tokens)
	s.analytics.recordLocal(model, tokens)
}

// LocalTokens returns the running total of real tokens served by local nodes.
// Exported only so integration tests in internal/proxy can verify that
// streamed responses produce real token counts; the value is otherwise
// exposed solely through the savings endpoint.
func (s *Server) LocalTokens() int64 {
	return atomic.LoadInt64(&s.localTokens)
}

// TrackCloudCostModel tracks a cloud request with model-level granularity.
// tokens is the real token count parsed from the provider response; 0 means
// the count was unavailable and no cost is recorded for the request.
func (s *Server) TrackCloudCostModel(model string, costPer1K float64, tokens int64) {
	atomic.AddInt64(&s.cloudCount, 1)
	atomic.AddInt64(&s.cloudTokens, tokens)
	cost := costPer1K * float64(tokens) / 1000.0
	s.mu.Lock()
	s.cloudSpentUSD += cost
	s.mu.Unlock()
	s.analytics.recordCloud(model, costPer1K, tokens)
}

func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	hourly := s.analytics.last24hBuckets()
	models := s.analytics.topModels()

	local := atomic.LoadInt64(&s.localCount)
	cloud := atomic.LoadInt64(&s.cloudCount)
	savedUSD, cloudSpent := s.savingsUSD()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"hourly":          hourly,
		"by_model":        models,
		"total_saved_usd": savedUSD,
		"total_spent_usd": cloudSpent,
		"local_requests":  local,
		"cloud_requests":  cloud,
		"since":           s.startTime.Format(time.RFC3339),
	})
}

// savingsUSD computes savings and cloud spend from real parsed token counts.
// Returns nil (JSON null) for a figure when requests exist but no token data
// was ever parsed — the UI renders that as "—" instead of a fabricated number.
func (s *Server) savingsUSD() (saved, spent interface{}) {
	local := atomic.LoadInt64(&s.localCount)
	cloud := atomic.LoadInt64(&s.cloudCount)
	localTok := atomic.LoadInt64(&s.localTokens)
	cloudTok := atomic.LoadInt64(&s.cloudTokens)
	s.mu.RLock()
	cloudSpent := s.cloudSpentUSD
	s.mu.RUnlock()

	// Savings = what local tokens would have cost at the reference cloud rate
	// (savings.reference_cost_per_1k in config, default 0.002).
	saved = float64(localTok) / 1000.0 * s.refCostPer1K
	if local > 0 && localTok == 0 {
		saved = nil
	}
	spent = cloudSpent
	if cloud > 0 && cloudTok == 0 {
		spent = nil
	}
	return saved, spent
}

func (s *Server) handleSavings(w http.ResponseWriter, r *http.Request) {
	local := atomic.LoadInt64(&s.localCount)
	cloud := atomic.LoadInt64(&s.cloudCount)
	savedUSD, cloudSpent := s.savingsUSD()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"local_requests":  local,
		"cloud_requests":  cloud,
		"cloud_spent_usd": cloudSpent,
		"saved_usd":       savedUSD,
		"total_requests":  local + cloud,
		"since":           s.startTime.Format(time.RFC3339),
	})
}

func (s *Server) handleCloudProviders(w http.ResponseWriter, r *http.Request) {
	type providerResp struct {
		Name            string  `json:"name"`
		Provider        string  `json:"provider"`
		BaseURL         string  `json:"base_url"`
		DefaultModel    string  `json:"default_model"`
		CostPer1KTokens float64 `json:"cost_per_1k_tokens"`
		Enabled         bool    `json:"enabled"`
	}
	s.mu.RLock()
	providers := make([]providerResp, 0, len(s.cfg.CloudProviders))
	for _, cp := range s.cfg.CloudProviders {
		providers = append(providers, providerResp{
			Name:            cp.Name,
			Provider:        cp.Provider,
			BaseURL:         cp.BaseURL,
			DefaultModel:    cp.DefaultModel,
			CostPer1KTokens: cp.CostPer1KTokens,
			Enabled:         cp.Enabled,
		})
	}
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(providers)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	type nodeInfo struct {
		Name    string `json:"name"`
		Healthy bool   `json:"healthy"`
	}
	type modelEntry struct {
		Name       string     `json:"name"`
		SizeVRAM   int64      `json:"size_vram"`
		Nodes      []nodeInfo `json:"nodes"`
		WarmCount  int        `json:"warm_count"`
		TotalNodes int        `json:"total_nodes"`
	}

	nodes := s.router.Nodes()
	modelMap := make(map[string]*modelEntry)

	for _, n := range nodes {
		n.RLock()
		for _, m := range n.LoadedModels {
			if modelMap[m.Name] == nil {
				modelMap[m.Name] = &modelEntry{
					Name:     m.Name,
					SizeVRAM: m.SizeVRAM,
				}
			}
			modelMap[m.Name].Nodes = append(modelMap[m.Name].Nodes, nodeInfo{
				Name:    n.Name,
				Healthy: n.Healthy,
			})
			if n.Healthy {
				modelMap[m.Name].WarmCount++
			}
		}
		n.RUnlock()
	}

	totalHealthy := 0
	for _, n := range nodes {
		n.RLock()
		if n.Healthy {
			totalHealthy++
		}
		n.RUnlock()
	}

	entries := make([]modelEntry, 0, len(modelMap))
	for _, v := range modelMap {
		v.TotalNodes = totalHealthy
		entries = append(entries, *v)
	}

	// Sort by warm_count desc, then name asc
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].WarmCount > entries[i].WarmCount ||
				(entries[j].WarmCount == entries[i].WarmCount && entries[j].Name < entries[i].Name) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models":        entries,
		"total_models":  len(entries),
		"total_nodes":   len(nodes),
		"healthy_nodes": totalHealthy,
	})
}

// handleNodePull triggers a model pull on a specific node via POST /api/pull.
// Accepts: {"model": "llama3:8b"}
// Returns 200 {"ok":true,"node":"...","model":"..."} on success.
func (s *Server) handleNodePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte(`{"error":"method not allowed"}`))
		return
	}

	// Go 1.22+ ServeMux populates PathValue from the {name} wildcard.
	// Manual parse covers the /admin/v1/ variant registered via strings.Replace.
	nodeName := r.PathValue("name")
	if nodeName == "" {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		for i, p := range parts {
			if p == "nodes" && i+2 < len(parts) && parts[i+2] == "pull" {
				nodeName = parts[i+1]
				break
			}
		}
	}

	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"missing or invalid model field"}`))
		return
	}

	urls := s.router.NodeURLs()
	nodeURL, ok := urls[nodeName]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":"node %q not found"}`, nodeName)
		return
	}

	pullBody, err := json.Marshal(map[string]interface{}{
		"model":  body.Model,
		"stream": false,
	})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"marshal error: %s"}`, err.Error())
		return
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, nodeURL+"/api/pull", bytes.NewReader(pullBody))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"pull node %s: %s"}`, nodeName, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"pull node %s: %s"}`, nodeName, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"pull node %s: upstream returned %d"}`, nodeName, resp.StatusCode)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    true,
		"node":  nodeName,
		"model": body.Model,
	})
}

// handleAudit queries the audit log with optional filters.
// GET /admin/v1/audit?limit=100&model=llama3&key=prod&cloud=true&since=2026-05-23T00:00:00Z
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if s.auditLog == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "audit log not configured"})
		return
	}
	q := r.URL.Query()

	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 1 {
				n = 1
			}
			if n > 1000 {
				n = 1000
			}
			limit = n
		} else {
			http.Error(w, `{"error":"invalid limit"}`, http.StatusBadRequest)
			return
		}
	}

	opts := audit.QueryOptions{
		Limit: limit,
		Model: q.Get("model"),
		Key:   q.Get("key"),
	}

	if v := q.Get("cloud"); v != "" {
		b := v == "true"
		opts.Cloud = &b
	}

	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, `{"error":"invalid since: use RFC3339"}`, http.StatusBadRequest)
			return
		}
		opts.Since = t
	}

	entries, err := s.auditLog.Query(opts)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []audit.Entry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entries":   entries,
		"total":     len(entries),
		"truncated": len(entries) == limit,
	})
}

// handleAnalyticsExport serves analytics data as CSV or JSON.
// Query params: format=csv|json (default json), type=hourly|models (default hourly).
func (s *Server) handleAnalyticsExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	exportType := r.URL.Query().Get("type")
	if exportType == "" {
		exportType = "hourly"
	}

	today := time.Now().UTC().Format("2006-01-02")

	if format == "csv" {
		switch exportType {
		case "models":
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ollama-mesh-models-%s.csv"`, today))
			cw := csv.NewWriter(w)
			_ = cw.Write([]string{"model", "local_requests", "cloud_requests", "local_pct", "saved_usd"})
			for _, m := range s.analytics.topModels() {
				total := m.Local + m.Cloud
				pct := int64(0)
				if total > 0 {
					pct = m.Local * 100 / total
				}
				_ = cw.Write([]string{
					m.Model,
					strconv.FormatInt(m.Local, 10),
					strconv.FormatInt(m.Cloud, 10),
					strconv.FormatInt(pct, 10),
					strconv.FormatFloat(m.SavedUSD, 'f', 6, 64),
				})
			}
			cw.Flush()
		default: // hourly
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ollama-mesh-analytics-%s.csv"`, today))
			cw := csv.NewWriter(w)
			_ = cw.Write([]string{"hour", "local_requests", "cloud_requests", "saved_usd", "spent_usd"})
			for _, b := range s.analytics.last24hBuckets() {
				_ = cw.Write([]string{
					b.Hour,
					strconv.FormatInt(b.Local, 10),
					strconv.FormatInt(b.Cloud, 10),
					strconv.FormatFloat(b.SavedUSD, 'f', 6, 64),
					strconv.FormatFloat(b.SpentUSD, 'f', 6, 64),
				})
			}
			cw.Flush()
		}
		return
	}

	// JSON output
	w.Header().Set("Content-Type", "application/json")
	switch exportType {
	case "models":
		json.NewEncoder(w).Encode(map[string]interface{}{
			"by_model": s.analytics.topModels(),
		})
	default: // hourly
		json.NewEncoder(w).Encode(map[string]interface{}{
			"hourly": s.analytics.last24hBuckets(),
		})
	}
}

// handleModelFit computes per-model VRAM fit status for every node.
// GET /admin/nodes/model-fit (also /admin/v1/nodes/model-fit)
//
// For each node it fetches /api/tags (with 30s cache in the router) to get all
// downloaded models and their disk sizes, combines that with the VRAM state
// from the last /api/ps poll, and classifies each model as:
//
//	green   - fits comfortably (model*1.15 <= free_vram*0.85)
//	yellow  - tight fit      (model*1.15 <= free_vram)
//	red     - won't fit      (model*1.15 > free_vram)
//	unknown - can't determine free VRAM
func (s *Server) handleModelFit(w http.ResponseWriter, r *http.Request) {
	type modelFitEntry struct {
		Name              string `json:"name"`
		SizeBytes         int64  `json:"size_bytes"`
		VRAMEstimateBytes int64  `json:"vram_estimate_bytes"`
		Fit               string `json:"fit"`
		Loaded            bool   `json:"loaded"`
	}
	type nodeFitEntry struct {
		Name           string          `json:"name"`
		URL            string          `json:"url"`
		VRAMFreeBytes  int64           `json:"vram_free_bytes"`
		VRAMTotalBytes int64           `json:"vram_total_bytes"`
		VRAMSource     string          `json:"vram_source"`
		Models         []modelFitEntry `json:"models"`
	}

	nodes := s.router.Nodes()
	result := make([]nodeFitEntry, 0, len(nodes))

	for _, n := range nodes {
		n.RLock()
		nodeURL := n.URL
		nodeName := n.Name
		vramTotalMB := n.VRAMTotalMB
		vramUsedMBFromPS := int64(0)
		loadedSet := make(map[string]bool)
		for _, m := range n.LoadedModels {
			loadedSet[m.Name] = true
			vramUsedMBFromPS += m.SizeVRAM / (1024 * 1024)
		}
		n.RUnlock()

		// Determine free VRAM and source label.
		var vramFreeBytes int64
		var vramTotalBytes int64
		vramSource := "unknown"

		if vramTotalMB > 0 {
			// nvidia-smi data is available on this host.
			vramTotalBytes = vramTotalMB * 1024 * 1024
			// Use nvidia-smi total minus what /api/ps says is loaded.
			vramUsedBytes := vramUsedMBFromPS * 1024 * 1024
			vramFreeBytes = vramTotalBytes - vramUsedBytes
			if vramFreeBytes < 0 {
				vramFreeBytes = 0
			}
			vramSource = "nvidia-smi"
		} else if vramUsedMBFromPS > 0 {
			// No nvidia-smi but we have ps data — use loaded model VRAM as lower bound.
			vramTotalBytes = 0
			vramFreeBytes = 0
			vramSource = "inferred"
		}

		// Fetch downloaded models from /api/tags (cached 30s in router).
		tagModels, err := s.router.FetchModelTags(nodeURL)
		if err != nil {
			// Node unreachable — emit an empty entry so the UI still shows the node.
			result = append(result, nodeFitEntry{
				Name:           nodeName,
				URL:            nodeURL,
				VRAMFreeBytes:  vramFreeBytes,
				VRAMTotalBytes: vramTotalBytes,
				VRAMSource:     vramSource,
				Models:         []modelFitEntry{},
			})
			continue
		}

		models := make([]modelFitEntry, 0, len(tagModels))
		for _, tm := range tagModels {
			estimate := int64(float64(tm.Size) * 1.15)
			fit := "unknown"
			if vramSource != "unknown" && vramSource != "inferred" {
				switch {
				case estimate <= int64(float64(vramFreeBytes)*0.85):
					fit = "green"
				case estimate <= vramFreeBytes:
					fit = "yellow"
				default:
					fit = "red"
				}
			}
			models = append(models, modelFitEntry{
				Name:              tm.Name,
				SizeBytes:         tm.Size,
				VRAMEstimateBytes: estimate,
				Fit:               fit,
				Loaded:            loadedSet[tm.Name],
			})
		}

		result = append(result, nodeFitEntry{
			Name:           nodeName,
			URL:            nodeURL,
			VRAMFreeBytes:  vramFreeBytes,
			VRAMTotalBytes: vramTotalBytes,
			VRAMSource:     vramSource,
			Models:         models,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"nodes": result,
	})
}

// handleHAPeers returns the HA peer reachability snapshot. Requires admin auth.
func (s *Server) handleHAPeers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.haMonitor == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": false,
			"peers":   []struct{}{},
		})
		return
	}
	statuses := s.haMonitor.PeerStatuses()
	type peerEntry struct {
		URL       string `json:"url"`
		Reachable bool   `json:"reachable"`
	}
	peers := make([]peerEntry, 0, len(statuses))
	for peerURL, reachable := range statuses {
		peers = append(peers, peerEntry{URL: peerURL, Reachable: reachable})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].URL < peers[j].URL })
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled": true,
		"peers":   peers,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	nodes := s.router.Nodes()
	total := len(nodes)
	healthy := 0
	for _, n := range nodes {
		n.RLock()
		if n.Healthy {
			healthy++
		}
		n.RUnlock()
	}

	uptimeSecs := int(time.Since(s.startTime).Seconds())

	status := "ok"
	if total > 0 && healthy == 0 {
		status = "degraded"
	}

	s.mu.RLock()
	proxyPort := s.cfg.Proxy.Port
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if status == "degraded" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         status,
		"version":        s.version,
		"proxy_port":     proxyPort,
		"uptime_seconds": uptimeSecs,
		"nodes": map[string]int{
			"total":   total,
			"healthy": healthy,
		},
	})
}
