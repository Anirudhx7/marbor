package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/ha"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

//go:embed web/dist
var webFS embed.FS

type ctxKey string

const ctxKeyUsername ctxKey = "username"

func extractBearerToken(hdr string) string {
	parts := strings.SplitN(hdr, " ", 2)
	if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
		return parts[1]
	}
	return ""
}

// writeJSONError writes a well-formed {"error": msg} JSON body with the given
// status code. Use this (rather than hand-building JSON with fmt.Sprintf and
// %q) whenever msg embeds a runtime value: %q already wraps its argument in
// its own literal quote characters, so splicing it into a template that is
// itself already inside a JSON string (e.g. `{"error":"node %q not found"}`)
// produces invalid JSON  --  the embedded quotes prematurely close the JSON
// string and the frontend's res.json() throws, silently swallowing the real
// error message and falling back to a generic one. json.Marshal here escapes
// msg correctly no matter what it contains.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		b = []byte(`{"error":"internal error"}`)
	}
	w.Write(b)
}

type TokenEvent struct {
	Time   time.Time
	Tokens int64
}

type Server struct {
	router        *router.Router
	auth          *auth.Middleware
	cfg           config.Config
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
	st            store.Store // never nil; NopStore when persistence disabled
	demoMode      bool        // when true, login accepts admin/admin without DB
	loginLimiter  *loginRateLimiter
	logChan       chan store.RequestRecord
	logDone       chan struct{} // closed by Shutdown to signal drain-and-stop
	logWg         sync.WaitGroup
	pullsMu       sync.Mutex      // guards pullsInFlight
	pullsInFlight map[string]bool // "node|model" -> in progress; ephemeral, never persisted
	coldStarts    int64           // atomic - total cold start events
	warmHits      int64           // atomic - total warm hit events
	tokenEvents   []TokenEvent    // protected by mu
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

// SetDemoMode enables demo mode, where the dashboard login accepts admin/admin.
func (s *Server) SetDemoMode(v bool) { s.demoMode = v }

// AdminToken enables demo mode on the server and returns the static demo
// session token. Use only in tests to bypass DB-backed session auth when
// the authentication pathway itself is not under test.
func (s *Server) AdminToken() string {
	s.demoMode = true
	return "demo-session"
}

// SetStore replaces the persistence backend. Useful for tests and for wiring
// a real SQLite store after construction when the store isn't known yet.
// If st is nil, NopStore is used so callers never need to nil-check.
func (s *Server) SetStore(st store.Store) {
	if st == nil {
		st = store.NopStore{}
	}
	s.st = st
}

// LoadFromStore seeds in-memory state from the persistent store on startup.
// Errors are non-fatal  --  the server still runs, just with a cold cache.
func (s *Server) LoadFromStore() error {
	// Restore global counters.
	if c, err := s.st.GetCounters(); err == nil {
		atomic.StoreInt64(&s.localCount, c.LocalRequests)
		atomic.StoreInt64(&s.cloudCount, c.CloudRequests)
		atomic.StoreInt64(&s.localTokens, c.TotalTokens)
		s.mu.Lock()
		s.cloudSpentUSD = c.CloudSpentUSD
		s.mu.Unlock()
	} else {
		log.Printf("store: could not load counters: %v", err)
	}
	// Restore last 50 request log entries.
	if recs, err := s.st.LastRequests(50); err == nil {
		logs := make([]RequestLog, 0, len(recs))
		for _, rec := range recs {
			logs = append(logs, RequestLog{
				ID:      rec.ID,
				ApiKey:  rec.KeyName,
				Model:   rec.Model,
				Node:    rec.NodeName,
				Status:  strconv.Itoa(rec.StatusCode),
				Latency: int(rec.LatencyMs),
				Tokens:  rec.TokensUsed,
				Time:    rec.TS,
			})
		}
		s.mu.Lock()
		s.requests = logs
		s.mu.Unlock()
	} else {
		log.Printf("store: could not load request log: %v", err)
	}

	// Restore global and per-node metrics from persistent audit log on startup
	if entries, err := s.st.QueryAuditLog(store.AuditQuery{Limit: 10000}); err == nil {
		var historicCold int64
		var historicWarm int64
		nodeCold := make(map[string]int64)
		nodeWarm := make(map[string]int64)
		nodeLatency := make(map[string]int64)
		nodeLatencyCount := make(map[string]int64)

		for _, entry := range entries {
			if entry.Status == "loading" {
				historicCold++
				nodeCold[entry.Node]++
			} else if entry.Status == "warm" {
				historicWarm++
				nodeWarm[entry.Node]++
			}
			if !entry.Cloud {
				nodeLatency[entry.Node] += int64(entry.LatencyMs)
				nodeLatencyCount[entry.Node]++
			}
		}
		atomic.StoreInt64(&s.coldStarts, historicCold)
		atomic.StoreInt64(&s.warmHits, historicWarm)

		// Seed node-level counters
		for _, n := range s.router.Nodes() {
			n.Lock()
			n.ColdStarts = nodeCold[n.Name]
			n.WarmHits = nodeWarm[n.Name]
			n.LatencySumMs = nodeLatency[n.Name]
			n.LatencyCount = nodeLatencyCount[n.Name]
			n.Unlock()
		}
	} else {
		log.Printf("store: could not load audit log for metrics initialization: %v", err)
	}
	// Restore hourly analytics buckets so the dashboard's traffic chart shows
	// continuous history immediately after a restart instead of a gap (see
	// docs/LIMITATIONS.md "Analytics dashboard shows a gap after restart").
	// 24h matches the window rendered by last24hBuckets()/handleAnalytics.
	since := time.Now().UTC().Add(-24 * time.Hour)
	if buckets, err := s.st.HourlyBuckets(since); err == nil {
		s.analytics.restoreFromStore(buckets)
	} else {
		log.Printf("store: could not load hourly analytics buckets: %v", err)
	}
	return nil
}

// StartCounterFlush launches a background goroutine that persists global
// counters every 30 seconds. Call once after construction; ctx cancellation
// stops the ticker.
func (s *Server) StartCounterFlush(ctx context.Context) {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.flushCounters()
				return
			case <-t.C:
				s.flushCounters()
			}
		}
	}()
}

func (s *Server) flushCounters() {
	local := atomic.LoadInt64(&s.localCount)
	cloud := atomic.LoadInt64(&s.cloudCount)
	localTok := atomic.LoadInt64(&s.localTokens)
	s.mu.RLock()
	spent := s.cloudSpentUSD
	s.mu.RUnlock()
	_ = s.st.SetCounters(store.Counters{
		LocalRequests: local,
		CloudRequests: cloud,
		TotalTokens:   localTok,
		CloudSpentUSD: spent,
	})
}

type RequestLog struct {
	ID           string    `json:"id"`
	ApiKey       string    `json:"apiKey"`
	SourceIP     string    `json:"sourceIP"`
	Model        string    `json:"model"`
	Node         string    `json:"routedTo"`
	Status       string    `json:"status"`
	Latency      int       `json:"latency"`
	Tokens       int64     `json:"tokens"`
	TokensPerSec float64   `json:"tokensPerSec"`
	Time         time.Time `json:"time"`
}

type nodeResp struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Port             int                `json:"port"`
	GPUModel         string             `json:"gpuModel"`
	VRAMTotalMB      int64              `json:"vramTotalMB"`
	VRAMUsedMB       int64              `json:"vramUsedMB"`
	VRAMSource       string             `json:"vramSource"`
	PowerDrawW       float64            `json:"powerDrawW"`
	Temperature      *float64           `json:"temperature"`
	Runtime          string             `json:"runtime"`
	Health           string             `json:"health"`
	Draining         bool               `json:"draining"`
	DrainedReason    string             `json:"drainedReason,omitempty"`
	PrewarmDisabled  bool               `json:"prewarmDisabled"`
	Uptime           string             `json:"uptime"`
	LoadedModels     []router.ModelInfo `json:"loadedModels"`
	ActiveConns      int32              `json:"activeConns"`
	RequestsTotal    int64              `json:"requestsTotal"`
	HealthHistory    []float64          `json:"healthHistory"`
	PendingPrewarmMB int64              `json:"pendingPrewarmMB"`
	ColdStarts       int64              `json:"coldStarts"`
	TokensTotal      int64              `json:"tokensTotal"`
	AvgLatencyMs     float64            `json:"avgLatencyMs"`
	WarmHitRatio     float64            `json:"warmHitRatio"`
}

type SystemInfo struct {
	CPUCores   int           `json:"cpu_cores"`
	OS         string        `json:"os"`
	Arch       string        `json:"arch"`
	RAMTotalMB int64         `json:"ram_total_mb"`
	RAMFreeMB  int64         `json:"ram_free_mb"`
	GPUs       []sysGPUEntry `json:"gpus"`
	ServerTime string        `json:"server_time"`
	Timezone   string        `json:"timezone"`
}

type sysGPUEntry struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	VRAMTotalMB  int64    `json:"vram_total_mb"`
	VRAMFreeMB   int64    `json:"vram_free_mb"`
	VRAMSource   string   `json:"vram_source"`
	TemperatureC *float64 `json:"temperature_c"`
	PowerDrawW   *float64 `json:"power_draw_w"`
	Healthy      bool     `json:"healthy"`
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
	DailyUsdCap       float64  `json:"dailyUsdCap,omitempty"`
	MonthlyUsdCap     float64  `json:"monthlyUsdCap,omitempty"`
	Status            string   `json:"status"`
	AllowedModels     []string `json:"allowedModels"`
	ExpiresAt         string   `json:"expiresAt,omitempty"`
}

func NewServer(r *router.Router, a *auth.Middleware, cfg config.Config, st ...store.Store) *Server {
	// Mirror config.Validate()'s default so servers constructed from a zero
	// config (tests, embedded use) still value local tokens at a real rate.
	refRate := cfg.Savings.ReferenceCostPer1K
	if refRate <= 0 {
		refRate = 0.002
	}
	var stImpl store.Store = store.NopStore{}
	if len(st) > 0 && st[0] != nil {
		stImpl = st[0]
	}
	s := &Server{
		router:        r,
		auth:          a,
		cfg:           cfg,
		version:       "dev",
		configPath:    "config.yaml",
		refCostPer1K:  refRate,
		startTime:     time.Now(),
		analytics:     newAnalyticsStore(refRate),
		st:            stImpl,
		loginLimiter:  newLoginRateLimiter(),
		logChan:       make(chan store.RequestRecord, 5000),
		logDone:       make(chan struct{}),
		pullsInFlight: make(map[string]bool),
	}
	s.ensureAdminUser()
	s.logWg.Add(1)
	go s.startAsyncLogger()
	return s
}

func (s *Server) startAsyncLogger() {
	defer s.logWg.Done()
	for {
		select {
		case rec := <-s.logChan:
			_ = s.st.AppendRequest(rec)
		case <-s.logDone:
			// Drain whatever is already buffered, then stop. logChan is
			// never closed (LogRequest keeps sending on it via a
			// non-blocking select even after Shutdown), so this only
			// races benignly: any record enqueued after the drain below
			// just sits unread, it never panics on a closed channel.
			for {
				select {
				case rec := <-s.logChan:
					_ = s.st.AppendRequest(rec)
				default:
					return
				}
			}
		}
	}
}

// Shutdown drains any in-flight request logs and stops the async logger.
// Call this after the HTTP servers have stopped accepting new requests
// and before closing the store, or the logger can still be writing
// through s.st after it's been closed.
func (s *Server) Shutdown() {
	close(s.logDone)
	s.logWg.Wait()
}

// StartPeriodicCleanup launches a background goroutine that prunes expired
// user sessions every 12 hours. Call once after construction; ctx
// cancellation stops the ticker, mirroring StartCounterFlush.
func (s *Server) StartPeriodicCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.st.PruneExpiredUserSessions()
			}
		}
	}()
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
	reg("PATCH /admin/nodes/{name}", s.cors(s.adminAuth(s.handlePatchNode)))
	reg("POST /admin/nodes", s.cors(s.adminAuth(s.handleAddNode)))
	reg("DELETE /admin/nodes/{name}", s.cors(s.adminAuth(s.handleRemoveNode)))
	reg("GET /admin/nodes/{name}/warmup", s.cors(s.adminAuth(s.handleGetNodeWarmup)))
	reg("PUT /admin/nodes/{name}/warmup", s.cors(s.adminAuth(s.handleSetNodeWarmup)))
	reg("GET /admin/nodes/{name}/pinned", s.cors(s.adminAuth(s.handleGetPinned)))
	reg("PUT /admin/nodes/{name}/pinned", s.cors(s.adminAuth(s.handleSetPinned)))
	reg("POST /admin/nodes/{name}/unload", s.cors(s.adminAuth(s.handleUnloadModel)))
	reg("GET /admin/schedules", s.cors(s.adminAuth(s.handleListSchedules)))
	reg("POST /admin/schedules", s.cors(s.adminAuth(s.handleCreateSchedule)))
	reg("DELETE /admin/schedules/{id}", s.cors(s.adminAuth(s.handleDeleteSchedule)))
	reg("PATCH /admin/schedules/{id}", s.cors(s.adminAuth(s.handlePatchSchedule)))

	reg("GET /admin/keys", s.cors(s.adminAuth(s.handleKeys)))
	reg("POST /admin/keys", s.cors(s.adminAuth(s.handleAddKey)))
	reg("PATCH /admin/keys/{name}", s.cors(s.adminAuth(s.handlePatchKey)))
	reg("DELETE /admin/keys/{name}", s.cors(s.adminAuth(s.handleRevokeKey)))

	reg("GET /admin/routing/rules", s.cors(s.adminAuth(s.handleRoutingRules)))
	reg("POST /admin/routing/rules", s.cors(s.adminAuth(s.handleAddRoutingRule)))
	reg("DELETE /admin/routing/rules/{id}", s.cors(s.adminAuth(s.handleRemoveRoutingRule)))
	reg("PUT /admin/routing/rules/{id}/toggle", s.cors(s.adminAuth(s.handleToggleRoutingRule)))
	reg("GET /admin/routing/strategy", s.cors(s.adminAuth(s.handleGetRoutingStrategy)))
	reg("PUT /admin/routing/strategy", s.cors(s.adminAuth(s.handleSetRoutingStrategy)))

	reg("GET /admin/settings", s.cors(s.adminAuth(s.handleSettings)))
	reg("PUT /admin/settings", s.cors(s.adminAuth(s.handleUpdateSettings)))

	reg("GET /admin/requests", s.cors(s.adminAuth(s.handleRequests)))
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
	reg("POST /admin/nodes/{name}/prewarm", s.cors(s.adminAuth(s.handleSetNodePrewarm)))
	reg("GET /admin/audit", s.cors(s.adminAuth(s.handleAudit)))
	reg("GET /admin/system-audit", s.cors(s.adminAuth(s.handleSystemAudit)))
	reg("GET /admin/nodes/model-fit", s.cors(s.adminAuth(s.handleModelFit)))
	reg("GET /admin/models/catalog", s.cors(s.adminAuth(s.handleModelCatalog)))
	reg("GET /admin/models/search", s.cors(s.adminAuth(s.handleModelSearch)))
	reg("GET /admin/models/repo", s.cors(s.adminAuth(s.handleModelRepo)))

	reg("GET /admin/model-config", s.cors(s.adminAuth(s.handleGetModelConfig)))
	reg("PUT /admin/model-config", s.cors(s.adminAuth(s.handleSetModelConfig)))
	reg("DELETE /admin/model-config", s.cors(s.adminAuth(s.handleDeleteModelConfig)))
	reg("GET /admin/model-configs", s.cors(s.adminAuth(s.handleListModelConfigs)))
	reg("GET /admin/model-config/capabilities", s.cors(s.adminAuth(s.handleModelConfigCapabilities)))

	reg("GET /admin/predictive/decisions", s.cors(s.adminAuth(s.handlePredictiveDecisions)))

	reg("GET /admin/ha/peers", s.cors(s.adminAuth(s.handleHAPeers)))
	reg("GET /admin/system-info", s.cors(s.adminAuth(s.handleSystemInfo)))
	reg("GET /admin/cloud-budget-status", s.cors(s.adminAuth(s.handleCloudBudgetStatus)))

	reg("GET /admin/warmup", s.cors(s.adminAuth(s.handleWarmupStatus)))
	reg("PUT /admin/warmup/predictive", s.cors(s.adminAuth(s.handleSetPredictiveEngine)))
	reg("POST /admin/warmup/ping", s.cors(s.adminAuth(s.handleWarmupPing)))

	reg("POST /admin/config/reload", s.cors(s.adminAuth(s.handleConfigReload)))
	reg("GET /admin/config", s.cors(s.adminAuth(s.handleGetConfig)))

	// Health check and login  --  no auth required.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /login", s.cors(s.handleLogin))
	mux.HandleFunc("POST /admin/login", s.cors(s.handleAdminLogin))
	reg("POST /admin/logout", s.cors(s.adminAuth(s.handleLogout)))
	reg("POST /admin/change-password", s.cors(s.adminAuth(s.handleChangePassword)))
	// Role-agnostic endpoints  --  any valid session (admin or user).
	mux.HandleFunc("POST /change-password", s.cors(s.sessionAuth(s.handleChangePassword)))
	mux.HandleFunc("POST /logout", s.cors(s.sessionAuth(s.handleLogout)))

	// User management (admin only, no /admin/* duplicate  --  these are v1-only)
	mux.HandleFunc("GET /admin/v1/users/pending-count", s.cors(s.adminAuth(s.handlePendingUserCount)))
	mux.HandleFunc("GET /admin/v1/users", s.cors(s.adminAuth(s.handleListUsers)))
	mux.HandleFunc("POST /admin/v1/users", s.cors(s.adminAuth(s.handleCreateUser)))
	mux.HandleFunc("POST /admin/v1/users/{id}/approve", s.cors(s.adminAuth(s.handleApproveUser)))
	mux.HandleFunc("POST /admin/v1/users/{id}/suspend", s.cors(s.adminAuth(s.handleSuspendUser)))
	mux.HandleFunc("DELETE /admin/v1/users/{id}", s.cors(s.adminAuth(s.handleDeleteUser)))
	mux.HandleFunc("PATCH /admin/v1/users/{id}", s.cors(s.adminAuth(s.handlePatchUser)))
	mux.HandleFunc("POST /admin/v1/users/{id}/reset-password", s.cors(s.adminAuth(s.handleResetUserPassword)))

	if sub, err := fs.Sub(webFS, "web/dist"); err == nil {
		mux.Handle("/assets/", s.noCache(http.FileServer(http.FS(sub))))
		// Serve root-level static files (favicon, manifest, etc.) that would
		// otherwise fall through to the SPA catch-all and return index.html.
		for _, f := range []string{"/favicon.svg", "/favicon.ico", "/manifest.json", "/robots.txt"} {
			mux.Handle(f, s.noCache(http.FileServer(http.FS(sub))))
		}
	} else {
		fmt.Println("warn: failed to embed web UI:", err)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve index.html for SPA routes; block unknown /admin/* API paths.
		// /admin/login is a frontend SPA route, not an API path  --  let it through.
		// Block unknown /admin/* API paths; /admin/login is a SPA route.
		// /login and /change-password are API endpoints registered above  --  they
		// never reach this catch-all, so no special-casing needed here.
		if strings.HasPrefix(r.URL.Path, "/admin") && r.URL.Path != "/admin/login" {
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

// sessionAuth accepts any valid session (any role). Use for endpoints that
// non-admin users must also reach (change-password, logout).
func (s *Server) sessionAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := extractBearerToken(r.Header.Get("Authorization"))
		if token != "" {
			if session, found, err := s.st.GetUserSession(token); err == nil && found {
				if session.MustChangePassword {
					p := r.URL.Path
					allowed := p == "/change-password" ||
						p == "/admin/v1/change-password" || p == "/admin/change-password" ||
						p == "/logout" || p == "/admin/v1/logout" || p == "/admin/logout"
					if !allowed {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						w.Write([]byte(`{"error":"password_change_required"}`))
						return
					}
				}
				r = r.WithContext(context.WithValue(r.Context(), ctxKeyUsername, session.Username))
				next(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}
}

func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := extractBearerToken(r.Header.Get("Authorization"))

		// Demo mode: accept the literal "demo-session" token so the GitHub Pages
		// demo works without a DB. Not a security concern  --  demo mode is opt-in,
		// explicitly labelled, and never ships with real credentials.
		if s.demoMode && token == "demo-session" {
			next(w, r)
			return
		}

		if token == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		session, found, err := s.st.GetUserSession(token)
		if err != nil || !found {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		if session.Role != "admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"admin role required"}`))
			return
		}
		if session.MustChangePassword {
			p := r.URL.Path
			allowed := p == "/admin/v1/change-password" || p == "/admin/change-password" ||
				p == "/admin/v1/logout" || p == "/admin/logout"
			if !allowed {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"password_change_required"}`))
				return
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyUsername, session.Username))
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
		// Empty history stays empty ([] in JSON)  --  the UI renders a "no data" state.
		hist := make([]float64, len(n.HealthHistory))
		copy(hist, n.HealthHistory)

		avgLatencyNode := 0.0
		latCount := atomic.LoadInt64(&n.LatencyCount)
		if latCount > 0 {
			avgLatencyNode = float64(atomic.LoadInt64(&n.LatencySumMs)) / float64(latCount)
		}
		coldNode := atomic.LoadInt64(&n.ColdStarts)
		warmNode := atomic.LoadInt64(&n.WarmHits)
		warmHitRatioNode := 0.0
		totalHitsNode := warmNode + coldNode
		if totalHitsNode > 0 {
			warmHitRatioNode = float64(warmNode) / float64(totalHitsNode)
		}

		out[i] = nodeResp{
			ID:               fmt.Sprintf("gpu-%d", i),
			Name:             n.Name,
			Port:             port,
			GPUModel:         n.GPUModel,
			VRAMTotalMB:      n.VRAMTotalMB,
			VRAMUsedMB:       n.VRAMUsedMB,
			VRAMSource:       n.VRAMSource,
			PowerDrawW:       n.PowerDrawW,
			Temperature:      n.Temperature,
			Runtime:          n.Runtime,
			Health:           health,
			Draining:         n.Draining,
			DrainedReason:    n.DrainedReason,
			PrewarmDisabled:  n.PrewarmDisabled,
			Uptime:           n.Uptime,
			LoadedModels:     safeModelInfoSlice(n.LoadedModels),
			ActiveConns:      atomic.LoadInt32(&n.ActiveConns),
			RequestsTotal:    atomic.LoadInt64(&n.RequestsTotal),
			HealthHistory:    hist,
			PendingPrewarmMB: s.router.PendingPrewarmBytes(n.Name) / (1024 * 1024),
			ColdStarts:       coldNode,
			TokensTotal:      atomic.LoadInt64(&n.TokensTotal),
			AvgLatencyMs:     avgLatencyNode,
			WarmHitRatio:     warmHitRatioNode,
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
			ID:               fmt.Sprintf("gpu-%d", i),
			Name:             n.Name,
			Port:             port,
			GPUModel:         n.GPUModel,
			VRAMTotalMB:      n.VRAMTotalMB,
			VRAMUsedMB:       n.VRAMUsedMB,
			VRAMSource:       n.VRAMSource,
			PowerDrawW:       n.PowerDrawW,
			Temperature:      n.Temperature,
			Runtime:          n.Runtime,
			Health:           health,
			Draining:         n.Draining,
			DrainedReason:    n.DrainedReason,
			PrewarmDisabled:  n.PrewarmDisabled,
			Uptime:           n.Uptime,
			LoadedModels:     safeModelInfoSlice(n.LoadedModels),
			ActiveConns:      atomic.LoadInt32(&n.ActiveConns),
			HealthHistory:    hist,
			PendingPrewarmMB: s.router.PendingPrewarmBytes(n.Name) / (1024 * 1024),
		}
		n.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
		return
	}
	writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	// Snapshot the config keys slice under lock.
	s.mu.RLock()
	cfgKeys := s.cfg.Auth.Keys
	s.mu.RUnlock()

	// Merge config keys with runtime keys from the store (added via admin API).
	// Config keys take precedence on name collision.
	keys := make([]config.KeyConfig, len(cfgKeys))
	copy(keys, cfgKeys)
	cfgNames := make(map[string]bool, len(cfgKeys))
	for _, k := range cfgKeys {
		cfgNames[k.Name] = true
	}
	if runtimeKeys, err := s.st.AllKeys(); err == nil {
		for _, rk := range runtimeKeys {
			if rk.Revoked || cfgNames[rk.Name] {
				continue
			}
			keys = append(keys, config.KeyConfig{
				Name:          rk.Name,
				Key:           rk.Key,
				RateLimit:     rk.RateLimit,
				DailyLimit:    rk.DailyLimit,
				MonthlyLimit:  rk.MonthlyLimit,
				DailyUsdCap:   rk.DailyUsdCap,
				MonthlyUsdCap: rk.MonthlyUsdCap,
				Models:        rk.Models,
			})
		}
	}
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
		dailyUsdCap, monthlyUsdCap := k.DailyUsdCap, k.MonthlyUsdCap
		if s.auth != nil {
			if kd, km, kok := s.auth.KeyUsdCaps(k.Name); kok {
				dailyUsdCap, monthlyUsdCap = kd, km
			}
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
			DailyUsdCap:       dailyUsdCap,
			MonthlyUsdCap:     monthlyUsdCap,
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

// handleRequests returns the request log in RequestEntry format for the dashboard.
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		ID        string    `json:"id"`
		Time      time.Time `json:"time"`
		KeyName   string    `json:"key_name"`
		SourceIP  string    `json:"source_ip"`
		Model     string    `json:"model"`
		Node      string    `json:"node"`
		Status    int       `json:"status"`
		LatencyMs int       `json:"latency_ms"`
		Cloud     bool      `json:"cloud"`
	}
	s.mu.RLock()
	reqs := make([]RequestLog, len(s.requests))
	copy(reqs, s.requests)
	s.mu.RUnlock()

	out := make([]entry, len(reqs))
	for i, req := range reqs {
		statusCode := 200
		if req.Status != "" {
			if code, err := strconv.Atoi(req.Status); err == nil {
				statusCode = code
			}
		}
		// Cloud nodes are stored as "cloud:<name>" (e.g. "cloud:openai").
		isCloud := strings.HasPrefix(req.Node, "cloud:")
		out[i] = entry{
			ID:        req.ID,
			Time:      req.Time,
			KeyName:   req.ApiKey,
			SourceIP:  req.SourceIP,
			Model:     req.Model,
			Node:      req.Node,
			Status:    statusCode,
			LatencyMs: req.Latency,
			Cloud:     isCloud,
		}
	}
	// Reverse so newest entries come first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	nodes := s.router.Nodes()
	totalConns := int32(0)
	online := 0
	draining := 0
	for _, n := range nodes {
		n.RLock()
		healthy := n.Healthy
		isDraining := n.Draining
		n.RUnlock()
		if healthy {
			online++
		}
		if isDraining {
			draining++
		}
		totalConns += atomic.LoadInt32(&n.ActiveConns)
	}

	// Compute average latency from the rolling last 50 requests
	s.mu.RLock()
	reqs := make([]RequestLog, len(s.requests))
	copy(reqs, s.requests)
	s.mu.RUnlock()

	var sumLatency int64
	var latencyCount int64
	for _, req := range reqs {
		if req.Latency > 0 {
			sumLatency += int64(req.Latency)
			latencyCount++
		}
	}
	avgLatency := 0.0
	if latencyCount > 0 {
		avgLatency = float64(sumLatency) / float64(latencyCount)
	}

	// Compute rolling tokens per minute
	s.mu.Lock()
	cutoff := time.Now().Add(-time.Minute)
	var keep []TokenEvent
	var tokensLastMin int64
	for _, e := range s.tokenEvents {
		if e.Time.After(cutoff) {
			tokensLastMin += e.Tokens
			keep = append(keep, e)
		}
	}
	s.tokenEvents = keep
	s.mu.Unlock()

	coldStarts := atomic.LoadInt64(&s.coldStarts)
	warmHits := atomic.LoadInt64(&s.warmHits)
	warmHitRatio := 0.0
	if warmHits+coldStarts > 0 {
		warmHitRatio = float64(warmHits) / float64(warmHits+coldStarts)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active_requests": totalConns,
		"nodes_online":    online,
		"nodes_draining":  draining,
		"total_nodes":     len(nodes),
		"queue_depth":     s.router.QueueDepth(),
		"avg_latency":     avgLatency,
		"tokens_per_min":  tokensLastMin,
		"cold_starts":     coldStarts,
		"warm_hit_ratio":  warmHitRatio,
	})
}

func (s *Server) handleWarmupStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.cfg.Warmup
	s.mu.RUnlock()

	predictiveEnabled := true
	if val, err := s.st.GetSetting("predictive_engine_enabled"); err == nil && val == "false" {
		predictiveEnabled = false
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled":                   cfg.Enabled,
		"interval_ms":               cfg.IntervalMs,
		"keep_alive":                cfg.KeepAlive,
		"models":                    cfg.Models,
		"predictive_engine_enabled": predictiveEnabled,
	})
}

func (s *Server) handleSetPredictiveEngine(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	val := "true"
	if !body.Enabled {
		val = "false"
	}
	if err := s.st.SetSetting("predictive_engine_enabled", val); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logSystemChange(r, "set_predictive_engine", "global", fmt.Sprintf("Enabled: %v", body.Enabled))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"predictive_engine_enabled": body.Enabled,
	})
}

// handlePredictiveDecisions returns the last 50 predictive-engine decisions,
// newest last. The engine is a stateless tick-and-act loop with no scheduled
// queue, so this is a log of what it actually decided on each tick, not a
// plan of what it will do next. Read-only; no config or routing change.
func (s *Server) handlePredictiveDecisions(w http.ResponseWriter, r *http.Request) {
	decisions := s.router.RecentPredictiveDecisions()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"decisions": decisions,
	})
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
// Equivalent to sending SIGHUP  --  useful in container environments where
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
	s.router.SetTimezone(newCfg.Timezone)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":"invalid request body"}`)
		return
	}
	if cfg.Name == "" || cfg.URL == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"name and url are required"}`))
		return
	}
	if err := config.ValidateNodeURL(cfg.URL); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"url must be http(s) with a host, and not a link-local/metadata address"}`))
		return
	}
	// Reject a URL that already belongs to a different, existing node rather
	// than silently registering the same physical backend twice under two
	// names (see Router.AddNode / FindNodeByURL for the normalized-URL
	// comparison and why this matters for capacity/eviction accounting).
	if existing, dup := s.router.FindNodeByURL(cfg.URL, cfg.Name); dup {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("url already registered as node %q", existing))
		return
	}
	if cfg.Runtime == "" {
		cfg.Runtime = "ollama"
	}
	switch cfg.Runtime {
	case "ollama", "vllm", "tgi", "llamacpp", "auto":
	default:
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown runtime %q (valid: ollama, vllm, tgi, llamacpp, auto)", cfg.Runtime))
		return
	}
	s.router.AddNode(cfg)
	var vramPtr *int64
	if cfg.VRAMTotalMB != 0 {
		v := cfg.VRAMTotalMB
		vramPtr = &v
	}
	_ = s.st.UpsertNode(store.NodeRecord{
		Name:        cfg.Name,
		URL:         cfg.URL,
		Runtime:     cfg.Runtime,
		VRAMTotalMB: vramPtr,
	})
	s.logSystemChange(r, "add_node", cfg.Name, fmt.Sprintf("URL: %s, Runtime: %s, VRAM: %dMB", cfg.URL, cfg.Runtime, cfg.VRAMTotalMB))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleRemoveNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"name is required"}`))
		return
	}
	s.router.RemoveNode(name)
	_ = s.st.DeleteNode(name)
	_ = s.st.SetSetting("warmup:node:"+name, "") // drop any warmup setting for the node
	s.logSystemChange(r, "remove_node", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleGetNodeWarmup returns the per-node runtime warmup setting.
func (s *Server) handleGetNodeWarmup(w http.ResponseWriter, r *http.Request) {
	nw := s.router.NodeWarmupSetting(r.PathValue("name"))
	models := nw.Models
	if models == nil {
		models = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": nw.Enabled, "models": models})
}

// handleSetNodeWarmup enables/disables proactive warmup for a node and sets
// which models to keep resident. Persisted to the KV store and applied live; an
// immediate warm cycle fires so the change takes effect now, not next tick.
func (s *Server) handleSetNodeWarmup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Enabled bool     `json:"enabled"`
		Models  []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request body"}`))
		return
	}
	raw, _ := json.Marshal(router.NodeWarmup{Enabled: body.Enabled, Models: body.Models})
	_ = s.st.SetSetting("warmup:node:"+name, string(raw))
	s.router.SetNodeWarmup(name, body.Enabled, body.Models)
	if body.Enabled && len(body.Models) > 0 {
		s.router.TriggerWarmup(context.Background())
	}
	s.logSystemChange(r, "set_node_warmup", name, fmt.Sprintf("Enabled: %v, Models: %v", body.Enabled, body.Models))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": body.Enabled, "models": body.Models})
}

// handleGetPinned returns the node's never-evict (pinned) model list.
func (s *Server) handleGetPinned(w http.ResponseWriter, r *http.Request) {
	models := s.router.PinnedModels(r.PathValue("name"))
	if models == nil {
		models = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

// handleSetPinned sets the node's never-evict model list (persisted + applied live).
func (s *Server) handleSetPinned(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request body"}`))
		return
	}
	raw, _ := json.Marshal(body.Models)
	_ = s.st.SetSetting("pinned:node:"+name, string(raw))
	s.router.SetPinnedModels(name, body.Models)
	s.logSystemChange(r, "set_pinned_models", name, fmt.Sprintf("Models: %v", body.Models))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": body.Models})
}

// handleUnloadModel evicts a single model from a node's VRAM on operator request
// (Ollama keep_alive:0). It frees VRAM immediately without draining the node or
// waiting for LRU pressure  --  the manual counterpart to auto-eviction.
func (s *Server) handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Model string `json:"model"`
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request body"}`))
		return
	}
	if body.Model == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"model is required"}`))
		return
	}
	found, err := s.router.UnloadModel(r.Context(), name, body.Model)
	if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	if errors.Is(err, router.ErrModelPinned) {
		// Pinning means "never evict/unload without an explicit unpin first"  --
		// this must be honored on the manual unload path exactly like it is on
		// auto-eviction. There is no force-override; unpin, then unload.
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	s.logSystemChange(r, "unload_model", name, fmt.Sprintf("Model: %s", body.Model))
	_ = json.NewEncoder(w).Encode(map[string]any{"node": name, "model": body.Model, "unloaded": true})
}

// --- Schedules: time-of-day warmup / unload / drain / undrain ---

func validHHMM(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	return err1 == nil && err2 == nil && h >= 0 && h < 24 && m >= 0 && m < 60
}

func (s *Server) persistSchedules(scheds []router.Schedule) {
	s.router.SetSchedules(scheds)
	if raw, err := json.Marshal(scheds); err == nil {
		_ = s.st.SetSetting("schedules", string(raw))
	}
}

// scheduleNodeExists reports whether name matches a currently registered node.
// Schedules against an unknown node silently no-op every time they fire (the
// scheduler can't find a target), so both create and patch reject them up
// front instead of accepting a schedule that will never do anything.
func (s *Server) scheduleNodeExists(name string) bool {
	for _, n := range s.router.Nodes() {
		if n.Name == name {
			return true
		}
	}
	return false
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	scheds := s.router.Schedules()
	if scheds == nil {
		scheds = []router.Schedule{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(scheds)
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var sc router.Schedule
	if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid request body"}`))
		return
	}
	if !router.ValidScheduleAction(sc.Action) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"action must be warmup, unload, drain, or undrain"}`))
		return
	}
	if sc.Node == "" || !validHHMM(sc.At) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"node is required and at must be HH:MM (24h)"}`))
		return
	}
	if !s.scheduleNodeExists(sc.Node) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("node %q is not registered", sc.Node))
		return
	}
	if (sc.Action == "warmup" || sc.Action == "unload") && len(sc.Models) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"at least one model is required for warmup and unload schedules"}`))
		return
	}
	sc.ID = fmt.Sprintf("sched-%d", time.Now().UnixNano())
	s.persistSchedules(append(s.router.Schedules(), sc))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sc)
}

func (s *Server) handlePatchSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var patch struct {
		Enabled *bool     `json:"enabled"`
		Action  *string   `json:"action"`
		Node    *string   `json:"node"`
		Models  *[]string `json:"models"`
		At      *string   `json:"at"`
		Days    *[]int    `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	cur := s.router.Schedules()
	idx := -1
	for i, sc := range cur {
		if sc.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("schedule %q not found", id))
		return
	}
	sc := cur[idx]
	if patch.Enabled != nil {
		sc.Enabled = *patch.Enabled
	}
	if patch.Action != nil {
		if !router.ValidScheduleAction(*patch.Action) {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid action %q", *patch.Action))
			return
		}
		sc.Action = *patch.Action
	}
	if patch.Node != nil {
		sc.Node = *patch.Node
	}
	if patch.Models != nil {
		sc.Models = *patch.Models
	}
	if patch.At != nil {
		if !validHHMM(*patch.At) {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"at must be HH:MM (24h)"}`, http.StatusBadRequest)
			return
		}
		sc.At = *patch.At
	}
	if patch.Days != nil {
		sc.Days = *patch.Days
	}
	// Re-validate the merged schedule so an edit can't leave it pointing at a
	// node that doesn't exist, or a warmup/unload schedule with no models  --
	// both of which would fire "successfully" every tick and silently do
	// nothing (see fireSchedule).
	if !s.scheduleNodeExists(sc.Node) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("node %q is not registered", sc.Node))
		return
	}
	if (sc.Action == "warmup" || sc.Action == "unload") && len(sc.Models) == 0 {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"at least one model is required for warmup and unload schedules"}`, http.StatusBadRequest)
		return
	}
	cur[idx] = sc
	s.persistSchedules(cur)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sc)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur := s.router.Schedules()
	out := make([]router.Schedule, 0, len(cur))
	found := false
	for _, sc := range cur {
		if sc.ID == id {
			found = true
			continue
		}
		out = append(out, sc)
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("schedule %q not found", id))
		return
	}
	s.persistSchedules(out)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDrainNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // optional body; empty is fine
	reason := body.Reason
	if reason == "" {
		reason = "manual"
	}
	if !s.router.DrainNode(name, reason) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	_ = s.st.SetNodeDrain(name, true, reason)
	s.logSystemChange(r, "drain_node", name, reason)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"node":%q,"draining":true,"reason":%q}`, name, reason)
}

func (s *Server) handleUndrainNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.router.UndrainNode(name) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	_ = s.st.SetNodeDrain(name, false, "")
	s.logSystemChange(r, "undrain_node", name, "")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"node":%q,"draining":false}`, name)
}

// handleSetNodePrewarm toggles whether the predictive engine may warm new
// models onto a node. Accepts {"disabled": true|false}. Live, in-memory only
// - unlike drain, this is never persisted to SQLite and always reverts to
// enabled (false) on restart.
func (s *Server) handleSetNodePrewarm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !s.router.SetPrewarmDisabled(name, body.Disabled) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	s.logSystemChange(r, "set_node_prewarm", name, fmt.Sprintf("Disabled: %v", body.Disabled))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"node":             name,
		"prewarm_disabled": body.Disabled,
	})
}

// handlePatchNode applies runtime metadata overrides to a node.
// PATCH /admin/nodes/{name}  --  accepts {"vram_total_mb":N,"gpu_model":"..."}
func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var patch router.NodePatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if !s.router.PatchNode(name, patch) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	_ = s.st.UpsertNodeOverride(name, patch.VRAMTotalMB, patch.GPUModel)
	s.logSystemChange(r, "patch_node", name, fmt.Sprintf("VRAMTotalMBChanged: %v, GPUModelChanged: %v", patch.VRAMTotalMB != nil, patch.GPUModel != nil))
	// Return the updated node.
	s.handleNode(w, r)
}

// handleGetModelConfig returns the configured default parameter profile for
// a model on a specific node. GET /admin/model-config?model=X&node=Y. 404 if
// no profile is configured for that exact pair  --  the caller sees the
// backend's own defaults apply (R1: never invents values the operator never
// set).
func (s *Server) handleGetModelConfig(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	node := r.URL.Query().Get("node")
	if model == "" {
		writeJSONError(w, http.StatusBadRequest, "model query param is required")
		return
	}
	if node == "" {
		writeJSONError(w, http.StatusBadRequest, "node query param is required")
		return
	}
	cfg, err := s.st.GetModelConfig(model, node)
	if err == store.ErrNotFound {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no config profile for model %q on node %q", model, node))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

// handleModelConfigCapabilities returns, for every known runtime, which
// ModelConfig fields actually take effect when injected  --  the single source
// of truth (store.SupportedFieldsFor) the UI reads to show only fields that
// are real for a given model/node's runtime, instead of hand-duplicating
// this list in TypeScript (which is exactly what caused this list to drift
// out of sync with the backend before). GET /admin/model-config/capabilities.
func (s *Server) handleModelConfigCapabilities(w http.ResponseWriter, r *http.Request) {
	out := map[string][]string{}
	for _, runtime := range []string{"ollama", "vllm", "tgi", "llamacpp"} {
		out[runtime] = store.SupportedFieldsFor(runtime)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleListModelConfigs returns every configured model profile.
// GET /admin/model-configs
func (s *Server) handleListModelConfigs(w http.ResponseWriter, r *http.Request) {
	cfgs, err := s.st.AllModelConfigs()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfgs == nil {
		cfgs = []store.ModelConfig{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"configs": cfgs})
}

// modelConfigRange bounds-checks a nullable float field, returning a 400-worthy
// error message when set and out of range. min/max are inclusive.
func modelConfigRange(name string, v *float64, min, max float64) string {
	if v == nil {
		return ""
	}
	if *v < min || *v > max {
		return fmt.Sprintf("%s must be between %g and %g", name, min, max)
	}
	return ""
}

// validateModelConfig rejects out-of-range values instead of silently
// clamping them (Audit & Triage Protocol: server-side validation, clear 400).
func validateModelConfig(cfg store.ModelConfig) string {
	if cfg.Model == "" {
		return "model is required"
	}
	if cfg.Node == "" {
		return "node is required"
	}
	for _, msg := range []string{
		modelConfigRange("temperature", cfg.Temperature, 0, 2),
		modelConfigRange("top_p", cfg.TopP, 0, 1),
		modelConfigRange("min_p", cfg.MinP, 0, 1),
		modelConfigRange("typical_p", cfg.TypicalP, 0, 1),
		modelConfigRange("presence_penalty", cfg.PresencePenalty, -2, 2),
		modelConfigRange("frequency_penalty", cfg.FrequencyPenalty, -2, 2),
		modelConfigRange("xtc_probability", cfg.XtcProbability, 0, 1),
	} {
		if msg != "" {
			return msg
		}
	}
	if cfg.Mirostat != nil && (*cfg.Mirostat < 0 || *cfg.Mirostat > 2) {
		return "mirostat must be 0, 1, or 2"
	}
	if cfg.RPM != nil && *cfg.RPM < 0 {
		return "rpm must be >= 0"
	}
	if cfg.TPM != nil && *cfg.TPM < 0 {
		return "tpm must be >= 0"
	}
	if cfg.NumCtx != nil && *cfg.NumCtx <= 0 {
		return "num_ctx must be > 0"
	}
	return ""
}

// handleSetModelConfig upserts a model's default parameter profile.
// PUT /admin/model-config, body = full store.ModelConfig JSON (model field required).
func (s *Server) handleSetModelConfig(w http.ResponseWriter, r *http.Request) {
	var cfg store.ModelConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if msg := validateModelConfig(cfg); msg != "" {
		writeJSONError(w, http.StatusBadRequest, msg)
		return
	}
	if err := s.st.SetModelConfig(cfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logSystemChange(r, "set_model_config", cfg.Model+"@"+cfg.Node, "updated model configuration profile")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

// handleDeleteModelConfig resets a model on a specific node to backend
// defaults by removing its configured profile. DELETE
// /admin/model-config?model=X&node=Y.
func (s *Server) handleDeleteModelConfig(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	node := r.URL.Query().Get("node")
	if model == "" {
		writeJSONError(w, http.StatusBadRequest, "model query param is required")
		return
	}
	if node == "" {
		writeJSONError(w, http.StatusBadRequest, "node query param is required")
		return
	}
	if err := s.st.DeleteModelConfig(model, node); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logSystemChange(r, "delete_model_config", model+"@"+node, "reset model configuration profile to defaults")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"model": model, "node": node, "status": "reset"})
}

// generateAPIKey creates a cryptographically random API key of the form sk-<name>-<48 hex chars>.
func generateAPIKey(name string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "sk-" + name + "-" + hex.EncodeToString(b)
}

// loginRateLimiter throttles admin login attempts per client IP to defend
// against brute-force credential guessing on the admin dashboard (port 8080).
// State is in-memory only (matches the rest of admin.go's in-process patterns)
// and resets on process restart  --  an acceptable tradeoff since a meaningful
// brute-force run takes far longer than typical restart cycles, and this is a
// throttle, not durable security state (rotate credentials if you suspect an
// actual compromise, restart alone doesn't help an attacker).
//
// Keyed by IP rather than username: IP-based throttling can't be used to lock
// out a legitimate admin by hammering their username from elsewhere, and it
// still slows down both single-source and low-volume distributed attempts.
type loginRateLimiter struct {
	mu           sync.RWMutex
	attempts     map[string]*loginAttemptState
	maxAttempts  int           // failures allowed within window before lockout
	window       time.Duration // sliding window in which failures accumulate
	lockDuration time.Duration // how long an IP stays locked out once tripped
	lastPrune    time.Time     // last time stale entries were swept
}

type loginAttemptState struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{
		attempts:     make(map[string]*loginAttemptState),
		maxAttempts:  5,
		window:       5 * time.Minute,
		lockDuration: 15 * time.Minute,
	}
}

// allow reports whether a login attempt from ip should proceed. If the IP is
// currently locked out it returns false and the remaining lockout duration.
func (l *loginRateLimiter) allow(ip string) (ok bool, retryAfter time.Duration) {
	now := time.Now()
	l.mu.RLock()
	st, exists := l.attempts[ip]
	l.mu.RUnlock()
	if !exists {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(st.lockedUntil) {
		return false, st.lockedUntil.Sub(now)
	}
	// Lockout expired (or none active)  --  if the failure window has also
	// elapsed, reset so old failures don't count against a fresh window.
	if now.Sub(st.windowStart) > l.window {
		st.failures = 0
		st.windowStart = now
	}
	return true, 0
}

// recordFailure registers a failed login attempt for ip and locks the IP out
// once maxAttempts is reached within the sliding window.
func (l *loginRateLimiter) recordFailure(ip string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	st, exists := l.attempts[ip]
	if !exists {
		st = &loginAttemptState{windowStart: now}
		l.attempts[ip] = st
	}
	if now.Sub(st.windowStart) > l.window {
		st.failures = 0
		st.windowStart = now
	}
	st.failures++
	if st.failures >= l.maxAttempts {
		st.lockedUntil = now.Add(l.lockDuration)
	}
}

// pruneLocked drops entries that are neither currently locked out nor within an
// active failure window, so a brute-force flood from many unique IPs can't grow
// the map without bound. Cheap: runs at most once per minute (caller holds the
// write lock). A dropped entry is harmless  --  the next failure just re-creates a
// fresh window for that IP.
func (l *loginRateLimiter) pruneLocked(now time.Time) {
	if now.Sub(l.lastPrune) < time.Minute {
		return
	}
	l.lastPrune = now
	for ip, st := range l.attempts {
		if now.After(st.lockedUntil) && now.Sub(st.windowStart) > l.window {
			delete(l.attempts, ip)
		}
	}
}

// recordSuccess clears any failure history for ip on a successful login.
func (l *loginRateLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, ip)
}

// clientIP extracts the client IP from r.RemoteAddr, stripping the port.
// Falls back to the raw RemoteAddr if it isn't in host:port form (e.g. in
// unit tests using httptest, which typically sets RemoteAddr as "ip:port"
// anyway, but this keeps things safe against unexpected formats).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleAdminLogin handles POST /admin/login  --  admin role required.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	s.handleLoginForRole(w, r, "admin")
}

// handleLogin handles POST /login  --  any active role accepted.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	s.handleLoginForRole(w, r, "")
}

func (s *Server) handleLoginForRole(w http.ResponseWriter, r *http.Request, requiredRole string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	ip := clientIP(r)
	if s.loginLimiter != nil {
		if ok, retryAfter := s.loginLimiter.allow(ip); !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
			w.WriteHeader(http.StatusTooManyRequests)
			// Deliberately generic: never reveal whether the username exists,
			// only that this IP is temporarily blocked from trying.
			w.Write([]byte(`{"error":"too many failed login attempts, try again later"}`))
			return
		}
	}

	// Demo mode: fixed admin/admin credentials, no DB needed.
	if s.demoMode {
		if req.Username == "admin" && req.Password == "admin" {
			if requiredRole != "" && requiredRole != "admin" {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"not an admin account"}`))
				return
			}
			if s.loginLimiter != nil {
				s.loginLimiter.recordSuccess(ip)
			}
			expiry := time.Now().Add(30 * 24 * time.Hour)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":                "demo-session",
				"role":                 "admin",
				"username":             "admin",
				"must_change_password": false,
				"expires_at":           expiry.Format(time.RFC3339),
			})
			return
		}
		if s.loginLimiter != nil {
			s.loginLimiter.recordFailure(ip)
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}

	user, err := s.st.GetUserByUsername(req.Username)
	if err != nil {
		if s.loginLimiter != nil {
			s.loginLimiter.recordFailure(ip)
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}

	if requiredRole != "" && user.Role != requiredRole {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"not an admin account"}`))
		return
	}

	switch user.Status {
	case "pending":
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"account pending approval"}`))
		return
	case "suspended":
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"account suspended"}`))
		return
	}

	if !verifyPassword(user.PasswordHash, req.Password) {
		if s.loginLimiter != nil {
			s.loginLimiter.recordFailure(ip)
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}

	if s.loginLimiter != nil {
		s.loginLimiter.recordSuccess(ip)
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sessionToken := hex.EncodeToString(b)
	expiry := time.Now().Add(30 * 24 * time.Hour)
	if err := s.st.CreateUserSession(store.UserSession{
		Token:              sessionToken,
		UserID:             user.ID,
		Role:               user.Role,
		Username:           user.Username,
		MustChangePassword: user.MustChangePassword,
		ExpiresAt:          expiry,
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	go s.st.PruneExpiredUserSessions()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token":                sessionToken,
		"role":                 user.Role,
		"username":             user.Username,
		"must_change_password": user.MustChangePassword,
		"expires_at":           expiry.Format(time.RFC3339),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r.Header.Get("Authorization"))
	_ = s.st.DeleteUserSession(token)
	_ = s.st.DeleteSession(token) // backward compat: also clear old admin_sessions
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{}`))
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
		// new_username kept for backward compat but ignored
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// Get calling username from context (set by adminAuth for session logins).
	username, _ := r.Context().Value(ctxKeyUsername).(string)
	if username == "" {
		// Legacy path: called with admin_token (no session context).
		s.handleChangePasswordLegacy(w, req.CurrentPassword, req.NewPassword)
		return
	}

	user, err := s.st.GetUserByUsername(username)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}

	// Skip current-password check on forced change (first-login flow).
	if !user.MustChangePassword {
		if req.CurrentPassword == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"current_password required"}`))
			return
		}
		if !verifyPassword(user.PasswordHash, req.CurrentPassword) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"wrong current password"}`))
			return
		}
	}
	if req.NewPassword == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"new_password required"}`))
		return
	}

	newHash, err := hashPassword(req.NewPassword)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"could not hash password"}`))
		return
	}
	user.PasswordHash = newHash
	user.Salt = ""
	user.MustChangePassword = false
	if err := s.st.UpdateUser(user); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"could not save credentials"}`))
		return
	}

	// Invalidate all sessions for this user and issue a fresh one.
	_ = s.st.DeleteUserSessionsByUserID(user.ID)
	b := make([]byte, 32)
	rand.Read(b)
	newToken := hex.EncodeToString(b)
	expiry := time.Now().Add(30 * 24 * time.Hour)
	_ = s.st.CreateUserSession(store.UserSession{
		Token:              newToken,
		UserID:             user.ID,
		Role:               user.Role,
		Username:           user.Username,
		MustChangePassword: false,
		ExpiresAt:          expiry,
	})
	json.NewEncoder(w).Encode(map[string]string{
		"token":      newToken,
		"expires_at": expiry.Format(time.RFC3339),
	})
}

func (s *Server) handleChangePasswordLegacy(w http.ResponseWriter, currentPass, newPass string) {
	creds, err := s.st.GetAdminCreds()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"could not load credentials"}`))
		return
	}
	if !verifyPassword(creds.PasswordHash, currentPass) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"wrong current password"}`))
		return
	}
	newHash, err := hashPassword(newPass)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"could not hash password"}`))
		return
	}
	if err := s.st.SetAdminCreds(store.AdminCreds{
		Username:     creds.Username,
		PasswordHash: newHash,
		Salt:         "",
	}); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"could not save credentials"}`))
		return
	}
	w.Write([]byte(`{}`))
}

// --- User management ---

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.st.ListUsers()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to list users"}`))
		return
	}
	if users == nil {
		users = []store.User{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(users)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"username required"}`))
		return
	}
	if req.Role != "admin" && req.Role != "user" {
		req.Role = "user"
	}
	status := "pending"
	if req.Role == "admin" {
		status = "active"
	}
	password := generatePassword(12)
	hash, err := hashPassword(password)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"could not hash password"}`))
		return
	}

	id, err := s.st.CreateUser(store.User{
		Username:           req.Username,
		Email:              req.Email,
		Role:               req.Role,
		Status:             status,
		PasswordHash:       hash,
		Salt:               "",
		MustChangePassword: true,
		CreatedAt:          time.Now(),
	})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(err.Error(), "UNIQUE") {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"username already exists"}`))
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"failed to create user"}`))
		}
		return
	}
	user, _ := s.st.GetUserByID(id)
	s.logSystemChange(r, "create_user", req.Username, fmt.Sprintf("Role: %s, Status: %s", req.Role, status))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	type resp struct {
		store.User
		InitialPassword string `json:"initial_password"`
	}
	json.NewEncoder(w).Encode(resp{User: user, InitialPassword: password})
}

func (s *Server) handleApproveUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid user id"}`))
		return
	}
	user, err := s.st.GetUserByID(id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}
	if user.Status != "pending" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"user is not pending approval"}`))
		return
	}

	var req struct {
		APIKeyName string `json:"api_key_name"`
		CreateKey  *struct {
			Name         string   `json:"name"`
			RateLimit    int      `json:"rate_limit_per_hour"`
			DailyLimit   int      `json:"daily_limit"`
			MonthlyLimit int      `json:"monthly_limit"`
			Models       []string `json:"models"`
		} `json:"create_key"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	approver, _ := r.Context().Value(ctxKeyUsername).(string)
	now := time.Now()
	var newKeyValue string

	if req.CreateKey != nil {
		keyName := req.CreateKey.Name
		if keyName == "" {
			keyName = user.Username + "-key"
		}
		newKeyValue = generateAPIKey(keyName)
		kc := config.KeyConfig{
			Name:         keyName,
			Key:          newKeyValue,
			RateLimit:    req.CreateKey.RateLimit,
			DailyLimit:   req.CreateKey.DailyLimit,
			MonthlyLimit: req.CreateKey.MonthlyLimit,
			Models:       req.CreateKey.Models,
		}
		if s.auth != nil {
			s.auth.AddKey(kc)
		}
		_ = s.st.UpsertKey(store.KeyRecord{
			Name:          kc.Name,
			Key:           kc.Key,
			RateLimit:     kc.RateLimit,
			DailyLimit:    kc.DailyLimit,
			MonthlyLimit:  kc.MonthlyLimit,
			DailyUsdCap:   kc.DailyUsdCap,
			MonthlyUsdCap: kc.MonthlyUsdCap,
			Models:        kc.Models,
		})
		req.APIKeyName = keyName
	}

	user.Status = "active"
	user.APIKeyName = req.APIKeyName
	user.ApprovedAt = &now
	user.ApprovedBy = approver
	if err := s.st.UpdateUser(user); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to update user"}`))
		return
	}
	s.logSystemChange(r, "approve_user", user.Username, fmt.Sprintf("APIKeyName: %s", req.APIKeyName))
	w.Header().Set("Content-Type", "application/json")
	type approveResp struct {
		User        store.User `json:"user"`
		APIKeyValue string     `json:"api_key_value,omitempty"`
	}
	json.NewEncoder(w).Encode(approveResp{User: user, APIKeyValue: newKeyValue})
}

func (s *Server) handleSuspendUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid user id"}`))
		return
	}
	user, err := s.st.GetUserByID(id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}
	callerUsername, _ := r.Context().Value(ctxKeyUsername).(string)
	if callerUsername != "" && user.Username == callerUsername {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"cannot suspend yourself"}`))
		return
	}
	user.Status = "suspended"
	if err := s.st.UpdateUser(user); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to suspend user"}`))
		return
	}
	_ = s.st.DeleteUserSessionsByUserID(id)
	s.logSystemChange(r, "suspend_user", user.Username, "")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{}`))
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid user id"}`))
		return
	}
	user, err := s.st.GetUserByID(id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}
	if user.Role == "admin" {
		if count, _ := s.st.CountAdminUsers(); count <= 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"cannot delete the last admin user"}`))
			return
		}
	}
	callerUsername, _ := r.Context().Value(ctxKeyUsername).(string)
	if callerUsername != "" && user.Username == callerUsername {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"cannot delete yourself"}`))
		return
	}
	_ = s.st.DeleteUserSessionsByUserID(id)
	if err := s.st.SoftDeleteUser(id, callerUsername); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to delete user"}`))
		return
	}
	s.logSystemChange(r, "delete_user", user.Username, "")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{}`))
}

func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid user id"}`))
		return
	}
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	user, err := s.st.GetUserByID(id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}
	if req.Role != "" && req.Role != user.Role {
		if user.Role == "admin" && req.Role == "user" {
			if count, _ := s.st.CountAdminUsers(); count <= 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"cannot demote the last admin"}`))
				return
			}
		}
		if req.Role == "admin" || req.Role == "user" {
			user.Role = req.Role
		}
	}
	if req.Email != "" {
		user.Email = req.Email
	}
	if err := s.st.UpdateUser(user); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to update user"}`))
		return
	}
	s.logSystemChange(r, "patch_user", user.Username, fmt.Sprintf("RoleChanged: %v, EmailChanged: %v", req.Role != "", req.Email != ""))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(user)
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid user id"}`))
		return
	}
	user, err := s.st.GetUserByID(id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}
	newPassword := generatePassword(12)
	hash, err := hashPassword(newPassword)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to hash password"}`))
		return
	}
	user.PasswordHash = hash
	user.Salt = ""
	user.MustChangePassword = true
	if err := s.st.UpdateUser(user); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to reset password"}`))
		return
	}
	_ = s.st.DeleteUserSessionsByUserID(id)
	s.logSystemChange(r, "reset_user_password", user.Username, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"initial_password": newPassword})
}

func (s *Server) handlePendingUserCount(w http.ResponseWriter, r *http.Request) {
	count, _ := s.st.PendingUserCount()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"count":%d}`, count)
}

// hashPassword hashes a plaintext password with bcrypt (cost=DefaultCost).
// The salt is embedded in the returned hash  --  no separate salt parameter needed.
// Replaces the broken SHA-256 loop (audit finding #1 / #9).
func hashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt: %w", err)
	}
	return string(h), nil
}

// verifyPassword checks a bcrypt hash against a plaintext password.
func verifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// generatePassword returns a cryptographically-random password of the given
// length using unbiased rejection sampling (audit finding #11).
func generatePassword(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	charsetLen := big.NewInt(int64(len(charset)))
	result := make([]byte, length)
	for i := range result {
		n, err := rand.Int(rand.Reader, charsetLen)
		if err != nil {
			log.Fatalf("admin: CSPRNG failed: %v", err)
		}
		result[i] = charset[n.Int64()]
	}
	return string(result)
}

// ensureAdminUser sets up the initial admin in the users table on first run.
// If the legacy admin_credentials table has data, it migrates that row.
// On a completely fresh install it generates a random password and logs it.
func (s *Server) ensureAdminUser() {
	if count, err := s.st.CountAdminUsers(); err == nil && count > 0 {
		return // already set up
	}
	// Migrate from legacy single-admin table if present.
	if has, _ := s.st.HasAdminCredentials(); has {
		if uname, hash, salt, err := s.st.GetLegacyAdminCreds(); err == nil {
			if uname == "" {
				uname = "admin"
			}
			if _, err2 := s.st.CreateUser(store.User{
				Username:           uname,
				Role:               "admin",
				Status:             "active",
				PasswordHash:       hash,
				Salt:               salt,
				MustChangePassword: false,
				CreatedAt:          time.Now(),
			}); err2 == nil {
				log.Printf("admin: migrated legacy credentials to users table (username: %s)", uname)
				return
			}
		}
	}
	// Fresh install: generate password and force change on first login.
	password := generatePassword(12)
	hash, hashErr := hashPassword(password)
	if hashErr != nil {
		log.Printf("admin: could not hash initial admin password: %v", hashErr)
		return
	}
	if _, err := s.st.CreateUser(store.User{
		Username:           "admin",
		Role:               "admin",
		Status:             "active",
		PasswordHash:       hash,
		Salt:               "",
		MustChangePassword: true,
		CreatedAt:          time.Now(),
	}); err != nil {
		log.Printf("admin: could not persist admin user: %v", err)
		return
	}
	log.Printf("admin dashboard - username: admin  password: %s  (you will be prompted to change this on first login)", password)
}

// ensureAdminCreds creates default admin credentials in the store if none exist.
// Called once on startup; prints the generated password so the operator can log in.
func (s *Server) ensureAdminCreds() {
	if _, err := s.st.GetAdminCreds(); err == nil {
		return // credentials already configured
	}
	password := generatePassword(12)
	hash, err := hashPassword(password)
	if err != nil {
		log.Printf("admin: could not hash initial admin credentials: %v", err)
		return
	}
	if err := s.st.SetAdminCreds(store.AdminCreds{
		Username:     "admin",
		PasswordHash: hash,
		Salt:         "",
	}); err != nil {
		log.Printf("admin: could not persist admin credentials: %v", err)
		return
	}
	log.Printf("admin dashboard login - username: admin  password: %s  (change this in Settings -> Admin Credentials)", password)
}

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
	if k.Name == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"name is required"}`))
		return
	}
	if k.RateLimit < 0 || k.DailyLimit < 0 || k.MonthlyLimit < 0 || k.DailyUsdCap < 0 || k.MonthlyUsdCap < 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"rate_limit, daily_limit, monthly_limit, daily_usd_cap, monthly_usd_cap must be >= 0"}`))
		return
	}
	if k.ExpiresAt != "" {
		exp, err1 := time.Parse("2006-01-02", k.ExpiresAt)
		if err1 != nil {
			exp, err1 = time.Parse(time.RFC3339, k.ExpiresAt)
		}
		if err1 != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"expires_at must be YYYY-MM-DD or RFC3339 format"}`))
			return
		}
		// Reject an already-past expiry: it would mint a key that can never
		// authenticate (keyExpired treats it as expired immediately), which is a
		// silent footgun rather than an intended action.
		if !exp.After(time.Now()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"expires_at is in the past"}`))
			return
		}
	}
	if k.Key == "" {
		k.Key = generateAPIKey(k.Name)
	}
	if s.auth != nil {
		s.auth.AddKey(k)
	}
	_ = s.st.UpsertKey(store.KeyRecord{
		Name:          k.Name,
		Key:           k.Key,
		RateLimit:     k.RateLimit,
		DailyLimit:    k.DailyLimit,
		MonthlyLimit:  k.MonthlyLimit,
		DailyUsdCap:   k.DailyUsdCap,
		MonthlyUsdCap: k.MonthlyUsdCap,
		Models:        k.Models,
		Revoked:       false,
	})
	s.logSystemChange(r, "add_key", k.Name, fmt.Sprintf("RateLimit: %d, DailyLimit: %d, MonthlyLimit: %d, DailyUsdCap: %f, MonthlyUsdCap: %f, Models: %v", k.RateLimit, k.DailyLimit, k.MonthlyLimit, k.DailyUsdCap, k.MonthlyUsdCap, k.Models))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(k)
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.auth != nil {
		s.auth.RevokeKey(name)
	}
	_ = s.st.RevokeKey(name)
	s.logSystemChange(r, "revoke_key", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// handlePatchKey updates mutable key settings (rate_limit, daily_limit,
// monthly_limit, models) without rotating the token or resetting counters.
// PATCH /admin/keys/{name}
func (s *Server) handlePatchKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var patch auth.KeyPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if s.auth == nil || !s.auth.PatchKey(name, patch) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("key %q not found", name))
		return
	}
	// Persist the patch changes to SQLite
	var keyRecord *store.KeyRecord
	keys, err := s.st.AllKeys()
	if err == nil {
		for _, k := range keys {
			if k.Name == name {
				keyRecord = &k
				break
			}
		}
	}
	if keyRecord != nil {
		if patch.RateLimit != nil {
			keyRecord.RateLimit = *patch.RateLimit
		}
		if patch.DailyLimit != nil {
			keyRecord.DailyLimit = *patch.DailyLimit
		}
		if patch.MonthlyLimit != nil {
			keyRecord.MonthlyLimit = *patch.MonthlyLimit
		}
		if patch.DailyUsdCap != nil {
			keyRecord.DailyUsdCap = *patch.DailyUsdCap
		}
		if patch.MonthlyUsdCap != nil {
			keyRecord.MonthlyUsdCap = *patch.MonthlyUsdCap
		}
		if patch.Models != nil {
			keyRecord.Models = patch.Models
		}
		_ = s.st.UpsertKey(*keyRecord)
	}
	s.logSystemChange(r, "patch_key", name, fmt.Sprintf("RateLimitChanged: %v, DailyLimitChanged: %v, MonthlyLimitChanged: %v, DailyUsdCapChanged: %v, MonthlyUsdCapChanged: %v, ModelsChanged: %v", patch.RateLimit != nil, patch.DailyLimit != nil, patch.MonthlyLimit != nil, patch.DailyUsdCap != nil, patch.MonthlyUsdCap != nil, patch.Models != nil))
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"key":%q,"updated":true}`, name)
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
	if rule.ID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"id is required"}`))
		return
	}
	if rule.Condition == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"condition is required"}`))
		return
	}
	s.router.AddRule(rule)
	_ = s.st.UpsertRoutingRule(store.RoutingRuleRecord{
		ID:        rule.ID,
		Condition: rule.Condition,
		Target:    rule.TargetNode,
		Priority:  rule.Priority,
		Enabled:   rule.Enabled,
		CreatedAt: time.Now(),
	})
	s.logSystemChange(r, "add_routing_rule", rule.ID, fmt.Sprintf("Condition: %s, Target: %s, Priority: %d, Enabled: %v", rule.Condition, rule.TargetNode, rule.Priority, rule.Enabled))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleRemoveRoutingRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.router.RemoveRule(id)
	_ = s.st.DeleteRoutingRule(id)
	s.logSystemChange(r, "remove_routing_rule", id, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleToggleRoutingRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.router.ToggleRule(id)
	// Reflect the new enabled state in SQLite by reading what the router now has.
	enabled := false
	for _, rule := range s.router.Rules() {
		if rule.ID == id {
			_ = s.st.SetRoutingRuleEnabled(id, rule.Enabled)
			enabled = rule.Enabled
			break
		}
	}
	s.logSystemChange(r, "toggle_routing_rule", id, fmt.Sprintf("Enabled: %v", enabled))
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
	if err := s.router.SetStrategy(req.Strategy); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	_ = s.st.SetSetting("routing_strategy", req.Strategy)
	s.logSystemChange(r, "set_routing_strategy", req.Strategy, "")
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
	if cfg.HuggingFace.Token != "" {
		cfg.HuggingFace.Token = "***"
	}

	username, _ := r.Context().Value(ctxKeyUsername).(string)
	if username == "" {
		username = "admin"
	}
	if val, err := s.st.GetSetting("pref:" + username + ":hide_demo_banner"); err == nil && val != "" {
		cfg.HideDemoBanner = (val == "true")
	} else {
		cfg.HideDemoBanner = false
	}
	if val, err := s.st.GetSetting("pref:" + username + ":hide_budget_banner"); err == nil && val != "" {
		cfg.HideBudgetBanner = (val == "true")
	} else {
		cfg.HideBudgetBanner = false
	}

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
	// The client echoes back the masked "***" placeholder (or omits the field
	// entirely) when the operator didn't change it; preserve the real token in
	// both cases instead of clobbering it with the mask.
	if incoming.HuggingFace.Token == "" || incoming.HuggingFace.Token == "***" {
		incoming.HuggingFace.Token = s.cfg.HuggingFace.Token
	}
	// The Settings page never sends these Routing sub-fields (they're managed
	// via their own dedicated endpoints/pages) - preserve them from the
	// current config instead of letting the wholesale s.cfg = incoming below
	// silently zero them out.
	if len(incoming.Routing.Rules) == 0 {
		incoming.Routing.Rules = s.cfg.Routing.Rules
	}
	if len(incoming.Routing.FallbackChains) == 0 {
		incoming.Routing.FallbackChains = s.cfg.Routing.FallbackChains
	}
	if !incoming.Routing.SessionAffinity {
		incoming.Routing.SessionAffinity = s.cfg.Routing.SessionAffinity
	}
	if incoming.Routing.ThermalWatchdog == (config.ThermalWatchdogConfig{}) {
		incoming.Routing.ThermalWatchdog = s.cfg.Routing.ThermalWatchdog
	}

	if err := incoming.Validate(); err != nil {
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("validation failed: %v", err), http.StatusBadRequest)
		return
	}

	s.cfg = incoming
	s.mu.Unlock()

	s.router.SetTimezone(incoming.Timezone)
	if err := s.st.SetSetting("timezone", incoming.Timezone); err != nil {
		log.Printf("admin: failed to persist timezone setting: %v", err)
	}

	// Phase 2: persist the remaining UI-editable scalars so they survive a
	// restart instead of reverting to config.yaml's defaults. Mirrors the
	// timezone pattern above; main.go's boot-load block applies these over
	// the loaded config.yaml value before the servers start.
	if incoming.Routing.Strategy != "" {
		if err := s.st.SetSetting("routing_strategy", incoming.Routing.Strategy); err != nil {
			log.Printf("admin: failed to persist routing_strategy setting: %v", err)
		}
	}
	scalarSettings := map[string]string{
		"proxy_port":            strconv.Itoa(incoming.Proxy.Port),
		"proxy_log_format":      incoming.Proxy.LogFormat,
		"litellm_url":           incoming.LiteLLM.URL,
		"litellm_enabled":       strconv.FormatBool(incoming.LiteLLM.Enabled),
		"cloud_daily_usd_cap":   strconv.FormatFloat(incoming.CloudBudget.DailyUSDCap, 'f', -1, 64),
		"cloud_monthly_usd_cap": strconv.FormatFloat(incoming.CloudBudget.MonthlyUSDCap, 'f', -1, 64),
		"metrics_enabled":       strconv.FormatBool(incoming.Metrics.Enabled),
		"metrics_port":          strconv.Itoa(incoming.Metrics.Port),
	}
	for key, val := range scalarSettings {
		if err := s.st.SetSetting(key, val); err != nil {
			log.Printf("admin: failed to persist %s setting: %v", key, err)
		}
	}

	username, _ := r.Context().Value(ctxKeyUsername).(string)
	if username == "" {
		username = "admin"
	}

	if err := s.st.SetSetting("pref:"+username+":hide_demo_banner", strconv.FormatBool(incoming.HideDemoBanner)); err != nil {
		log.Printf("admin: failed to persist hide_demo_banner setting: %v", err)
	}
	if err := s.st.SetSetting("pref:"+username+":hide_budget_banner", strconv.FormatBool(incoming.HideBudgetBanner)); err != nil {
		log.Printf("admin: failed to persist hide_budget_banner setting: %v", err)
	}

	s.logSystemChange(r, "update_settings", "global", fmt.Sprintf("Timezone: %s, AuthEnabled: %v, PollInterval: %dms, DailyCap: %f, MonthlyCap: %f", incoming.Timezone, incoming.Auth.Enabled != nil && *incoming.Auth.Enabled, incoming.Routing.PollIntervalMs, incoming.CloudBudget.DailyUSDCap, incoming.CloudBudget.MonthlyUSDCap))

	// Settings now persist to SQLite routing_rules/runtime_nodes/runtime_keys
	// tables on each mutation. Scalar settings migration to the settings table
	// completes in Phase 2. config.SaveConfig removed (audit findings #2, #10).
	w.WriteHeader(http.StatusOK)
}

func (s *Server) logSystemChange(r *http.Request, action, target, details string) {
	username, _ := r.Context().Value(ctxKeyUsername).(string)
	if username == "" {
		username = "admin"
	}
	source := r.RemoteAddr
	if host, _, err := net.SplitHostPort(source); err == nil {
		source = host
	}
	_ = s.st.AppendSystemAuditLog(store.SystemAuditEntry{
		Time:     time.Now(),
		Username: username,
		Action:   action,
		Target:   target,
		Details:  details,
		SourceIP: source,
	})
}

func (s *Server) LogRequest(apiKey, sourceIP, model, node, status string, latencyMs int, tokens int64) {
	var tps float64
	if tokens > 0 && latencyMs > 0 {
		tps = float64(tokens) / (float64(latencyMs) / 1000.0)
	}
	// Attribute token usage to the calling key for per-key analytics + cost.
	if s.auth != nil {
		s.auth.AddKeyTokens(apiKey, tokens)
	}
	now := time.Now()
	b := make([]byte, 4)
	var id string
	if _, err := rand.Read(b); err == nil {
		id = "req-" + hex.EncodeToString(b)
	} else {
		id = fmt.Sprintf("req-%x", now.UnixNano())
	}
	s.mu.Lock()
	s.requests = append(s.requests, RequestLog{
		ID:           id,
		ApiKey:       apiKey,
		SourceIP:     sourceIP,
		Model:        model,
		Node:         node,
		Status:       status,
		Latency:      latencyMs,
		Tokens:       tokens,
		TokensPerSec: tps,
		Time:         now,
	})
	if len(s.requests) > 50 {
		s.requests = s.requests[len(s.requests)-50:]
	}
	s.tokenEvents = append(s.tokenEvents, TokenEvent{Time: now, Tokens: tokens})
	s.mu.Unlock()

	if status == "loading" {
		atomic.AddInt64(&s.coldStarts, 1)
	} else if status == "warm" {
		atomic.AddInt64(&s.warmHits, 1)
	}

	if n := s.router.FindNode(node); n != nil {
		if status == "loading" {
			atomic.AddInt64(&n.ColdStarts, 1)
		} else if status == "warm" {
			atomic.AddInt64(&n.WarmHits, 1)
		}
		atomic.AddInt64(&n.TokensTotal, tokens)
		if latencyMs > 0 {
			atomic.AddInt64(&n.LatencySumMs, int64(latencyMs))
			atomic.AddInt64(&n.LatencyCount, 1)
		}
	}

	// Parse status code for the store record.
	statusCode := 200
	if status != "" && status != "200" {
		if code, err := strconv.Atoi(status); err == nil {
			statusCode = code
		}
	}

	select {
	case s.logChan <- store.RequestRecord{
		ID:         id,
		KeyName:    apiKey,
		Model:      model,
		NodeName:   node,
		StatusCode: statusCode,
		LatencyMs:  int64(latencyMs),
		TokensUsed: tokens,
		TS:         now,
	}:
	default:
		// Prevent blocking the proxy path if SQLite writes are completely backed up.
		log.Printf("async logger: queue full, dropped request log %s", id)
	}
}

// TrackLocalRequestModel tracks a local request with model-level granularity.
// tokens is the real token count parsed from the response (eval_count +
// prompt_eval_count); 0 means the count was unavailable and contributes
// nothing to savings. genDurationMs is Ollama's real eval_duration in
// milliseconds (generation time only, excluding prompt processing); 0 means
// unavailable (cloud responses never report it) and is excluded from the
// hourly tokens-per-second rollup rather than skewing it toward infinity.
func (s *Server) TrackLocalRequestModel(model string, tokens, genDurationMs int64) {
	atomic.AddInt64(&s.localCount, 1)
	atomic.AddInt64(&s.localTokens, tokens)
	s.analytics.recordLocal(model, tokens, genDurationMs)
	// Persist hourly bucket and model stat for this request.
	now := time.Now().UTC().Truncate(time.Hour)
	saved := s.refCostPer1K * float64(tokens) / 1000.0
	_ = s.st.UpsertHourlyBucket(store.HourlyBucket{
		Hour:          now,
		LocalRequests: 1,
		Tokens:        tokens,
		CostUSD:       0,
		GenDurationMs: genDurationMs,
	})
	_ = s.st.UpsertModelStat(store.ModelStat{
		Model:    model,
		Requests: 1,
		Tokens:   tokens,
		CostUSD:  saved,
	})
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
	// Persist hourly bucket and model stat for this request.
	now := time.Now().UTC().Truncate(time.Hour)
	_ = s.st.UpsertHourlyBucket(store.HourlyBucket{
		Hour:          now,
		CloudRequests: 1,
		Tokens:        tokens,
		CostUSD:       cost,
	})
	_ = s.st.UpsertModelStat(store.ModelStat{
		Model:    model,
		Requests: 1,
		Tokens:   tokens,
		CostUSD:  cost,
	})
}

// cloudSpendSince sums real CostUSD from persisted hourly buckets since the
// given time - the same figures already used for /admin/analytics, never an
// estimate. Returns 0 on a store error (fails open: a budget check that
// can't read its own data must not block cloud fallback).
func (s *Server) cloudSpendSince(since time.Time) float64 {
	buckets, err := s.st.HourlyBuckets(since)
	if err != nil {
		return 0
	}
	var total float64
	for _, b := range buckets {
		total += b.CostUSD
	}
	return total
}

// CloudBudgetExceeded reports whether cumulative cloud spend has reached the
// configured daily or monthly cap - the global cap (routing.cloud_budget in
// config) or, if keyName has its own cap set, that key's cap - and a
// human-readable reason if so. Global caps default to 0 (disabled); a key
// with no cap configured is only subject to the global check.
func (s *Server) CloudBudgetExceeded(keyName string) (bool, string) {
	dailyCap := s.cfg.CloudBudget.DailyUSDCap
	monthlyCap := s.cfg.CloudBudget.MonthlyUSDCap
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	if dailyCap > 0 {
		if spent := s.cloudSpendSince(dayStart); spent >= dailyCap {
			return true, fmt.Sprintf("daily cloud spend cap of $%.2f reached (spent $%.2f)", dailyCap, spent)
		}
	}
	if monthlyCap > 0 {
		if spent := s.cloudSpendSince(monthStart); spent >= monthlyCap {
			return true, fmt.Sprintf("monthly cloud spend cap of $%.2f reached (spent $%.2f)", monthlyCap, spent)
		}
	}

	if keyName == "" || s.auth == nil {
		return false, ""
	}
	keyDailyCap, keyMonthlyCap, ok := s.auth.KeyUsdCaps(keyName)
	if !ok || (keyDailyCap <= 0 && keyMonthlyCap <= 0) {
		return false, ""
	}
	if keyDailyCap > 0 {
		if spent, err := s.st.KeySpendSince(keyName, dayStart); err == nil && spent >= keyDailyCap {
			return true, fmt.Sprintf("key %q daily cloud spend cap of $%.2f reached (spent $%.2f)", keyName, keyDailyCap, spent)
		}
	}
	if keyMonthlyCap > 0 {
		if spent, err := s.st.KeySpendSince(keyName, monthStart); err == nil && spent >= keyMonthlyCap {
			return true, fmt.Sprintf("key %q monthly cloud spend cap of $%.2f reached (spent $%.2f)", keyName, keyMonthlyCap, spent)
		}
	}
	return false, ""
}

// BudgetEntry is one cloud-spend budget's current standing (global or a
// single key), used by the soft-budget warning banner. Pct is 0 when its
// cap is unset (0/disabled) - never divides by zero.
type BudgetEntry struct {
	Name         string  `json:"name,omitempty"`
	DailySpent   float64 `json:"dailySpent"`
	DailyCap     float64 `json:"dailyCap"`
	DailyPct     float64 `json:"dailyPct"`
	MonthlySpent float64 `json:"monthlySpent"`
	MonthlyCap   float64 `json:"monthlyCap"`
	MonthlyPct   float64 `json:"monthlyPct"`
}

// CloudBudgetStatusResp is the /admin/cloud-budget-status response: the
// global budget plus every key that has its own cap set. Keys with no cap
// configured are omitted - nothing to warn about.
type CloudBudgetStatusResp struct {
	SoftBudgetPct float64       `json:"softBudgetPct"`
	Global        BudgetEntry   `json:"global"`
	PerKey        []BudgetEntry `json:"perKey"`
}

func budgetPct(spent, cap float64) float64 {
	if cap <= 0 {
		return 0
	}
	return spent / cap
}

// handleCloudBudgetStatus reports current cloud spend against configured
// caps for the dashboard's soft-budget warning banner. Read-only; reuses the
// same spend figures CloudBudgetExceeded already computes - no new spend
// tracking, just packaging.
func (s *Server) handleCloudBudgetStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	s.mu.RLock()
	cfgKeys := s.cfg.Auth.Keys
	dailyCap, monthlyCap, softPct := s.cfg.CloudBudget.DailyUSDCap, s.cfg.CloudBudget.MonthlyUSDCap, s.cfg.CloudBudget.SoftBudgetPct
	s.mu.RUnlock()

	global := BudgetEntry{
		DailyCap:   dailyCap,
		MonthlyCap: monthlyCap,
	}
	global.DailySpent = s.cloudSpendSince(dayStart)
	global.MonthlySpent = s.cloudSpendSince(monthStart)
	global.DailyPct = budgetPct(global.DailySpent, global.DailyCap)
	global.MonthlyPct = budgetPct(global.MonthlySpent, global.MonthlyCap)

	var perKey []BudgetEntry
	seen := make(map[string]bool)
	addKeyStatus := func(name string) {
		if seen[name] || s.auth == nil {
			return
		}
		seen[name] = true
		daily, monthly, ok := s.auth.KeyUsdCaps(name)
		if !ok || (daily <= 0 && monthly <= 0) {
			return
		}
		e := BudgetEntry{Name: name, DailyCap: daily, MonthlyCap: monthly}
		e.DailySpent, _ = s.st.KeySpendSince(name, dayStart)
		e.MonthlySpent, _ = s.st.KeySpendSince(name, monthStart)
		e.DailyPct = budgetPct(e.DailySpent, e.DailyCap)
		e.MonthlyPct = budgetPct(e.MonthlySpent, e.MonthlyCap)
		perKey = append(perKey, e)
	}
	for _, k := range cfgKeys {
		addKeyStatus(k.Name)
	}
	if runtimeKeys, err := s.st.AllKeys(); err == nil {
		for _, rk := range runtimeKeys {
			if !rk.Revoked {
				addKeyStatus(rk.Name)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(CloudBudgetStatusResp{
		SoftBudgetPct: softPct,
		Global:        global,
		PerKey:        perKey,
	})
}

// ContextWindowFor returns the operator-declared context window (in tokens)
// for model from config.context_windows, and whether one is configured. A
// model with no declared window means the proxy performs no admission-time
// context-length check for it (fails open - R1: never guess a value that
// wasn't declared).
func (s *Server) ContextWindowFor(model string) (int, bool) {
	window, ok := s.cfg.ContextWindows[model]
	return window, ok
}

// ModelConfigFor returns the operator-configured default parameter profile
// for model on the given node, if one exists. Lets the proxy inject defaults
// without holding a direct store reference (mirrors ContextWindowFor's
// admin-as-facade shape).
func (s *Server) ModelConfigFor(model, node string) (store.ModelConfig, bool) {
	cfg, err := s.st.GetModelConfig(model, node)
	if err != nil {
		return store.ModelConfig{}, false
	}
	return cfg, true
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
// was ever parsed  --  the UI renders that as " -- " instead of a fabricated number.
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
		nodeURL := n.URL
		nodeName := n.Name
		nodeHealthy := n.Healthy
		// Track which models are warm (currently in VRAM) for this node.
		warmSet := make(map[string]bool, len(n.LoadedModels))
		for _, m := range n.LoadedModels {
			warmSet[m.Name] = true
			if modelMap[m.Name] == nil {
				modelMap[m.Name] = &modelEntry{
					Name:     m.Name,
					SizeVRAM: m.SizeVRAM,
				}
			}
			modelMap[m.Name].Nodes = append(modelMap[m.Name].Nodes, nodeInfo{
				Name:    nodeName,
				Healthy: nodeHealthy,
			})
			if nodeHealthy {
				modelMap[m.Name].WarmCount++
			}
		}
		n.RUnlock()

		// Also include models that are installed on disk but not currently loaded.
		// FetchModelTags queries /api/tags which returns all available models.
		if nodeHealthy && nodeURL != "" {
			if tags, err := s.router.FetchModelTags(nodeURL); err == nil {
				for _, tm := range tags {
					if warmSet[tm.Name] {
						continue // already added with warm count above
					}
					if modelMap[tm.Name] == nil {
						modelMap[tm.Name] = &modelEntry{Name: tm.Name}
					}
					modelMap[tm.Name].Nodes = append(modelMap[tm.Name].Nodes, nodeInfo{
						Name:    nodeName,
						Healthy: nodeHealthy,
					})
					// WarmCount stays 0: model is available but not in VRAM
				}
			}
		}
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

// nodePullTimeout bounds how long ollama-mesh waits for a model pull to
// finish on the target node before giving up and returning 502. handleNodePull
// calls Ollama's /api/pull with "stream":false, so  --  unlike a normal chat/
// generate request  --  the upstream sends no response at all (not even
// headers) until the *entire* download completes. Model pulls, especially
// Hugging Face-sourced GGUF files (fetched directly from huggingface.co
// rather than Ollama's CDN-backed registry), routinely take much longer than
// a typical multi-GB-per-minute registry pull. A short client timeout here
// aborts an otherwise-successful pull mid-download and surfaces to the admin
// UI as a spurious "Bad Gateway", even though the node is still working.
// A var (not const) so tests can override it to keep test runtimes short.
var nodePullTimeout = 2 * time.Hour

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
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
		return
	}

	// Dedup concurrent pulls of the same model on the same node. State is
	// ephemeral and in-memory only - it is never persisted and never wired
	// into placement/warm-residency scoring, it just prevents two admin
	// clicks from racing the same multi-GB download.
	pullKey := nodeName + "|" + body.Model
	s.pullsMu.Lock()
	if s.pullsInFlight[pullKey] {
		s.pullsMu.Unlock()
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("pull already in progress for %q on node %q", body.Model, nodeName))
		return
	}
	s.pullsInFlight[pullKey] = true
	s.pullsMu.Unlock()
	defer func() {
		s.pullsMu.Lock()
		delete(s.pullsInFlight, pullKey)
		s.pullsMu.Unlock()
	}()

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

	client := &http.Client{Timeout: nodePullTimeout}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, nodeURL+"/api/pull", bytes.NewReader(pullBody))
	if err != nil {
		log.Printf("handleNodePull: build request for node %s: %v", nodeName, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"pull failed for node %s"}`, nodeName)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("handleNodePull: request to node %s failed: %v", nodeName, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"pull failed for node %s"}`, nodeName)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"pull node %s: upstream returned %d"}`, nodeName, resp.StatusCode)
		return
	}

	s.logSystemChange(r, "pull_model", nodeName, fmt.Sprintf("Model: %s", body.Model))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    true,
		"node":  nodeName,
		"model": body.Model,
	})
}

// handleAudit queries the audit log with optional filters.
// GET /admin/v1/audit?limit=100&model=llama3&key=prod&node=gpu-node-01&status=client_error&cloud=true&since=2026-05-23T00:00:00Z&until=2026-05-24T00:00:00Z
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
		Node:  q.Get("node"),
	}

	if v := q.Get("status"); v != "" {
		switch v {
		case "success", "client_error", "server_error":
			opts.StatusCategory = v
		default:
			http.Error(w, `{"error":"invalid status: use success, client_error, or server_error"}`, http.StatusBadRequest)
			return
		}
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

	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, `{"error":"invalid until: use RFC3339"}`, http.StatusBadRequest)
			return
		}
		opts.Until = t
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

func (s *Server) handleSystemAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n > 0 && n <= 1000 {
				limit = n
			}
		}
	}
	entries, err := s.st.QuerySystemAuditLog(limit)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []store.SystemAuditEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
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
		rawVramSource := n.VRAMSource
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
			vramTotalBytes = vramTotalMB * 1024 * 1024
			// Use nvidia-smi total minus what /api/ps says is loaded.
			vramUsedBytes := vramUsedMBFromPS * 1024 * 1024
			vramFreeBytes = vramTotalBytes - vramUsedBytes
			if vramFreeBytes < 0 {
				vramFreeBytes = 0
			}
			if rawVramSource == "nvidia" {
				vramSource = "nvidia-smi"
			} else if rawVramSource == "declared" {
				vramSource = "declared"
			} else {
				vramSource = "nvidia-smi" // fallback
			}
		} else if vramUsedMBFromPS > 0 {
			// No nvidia-smi but we have ps data  --  use loaded model VRAM as lower bound.
			vramTotalBytes = 0
			vramFreeBytes = 0
			vramSource = "inferred"
		} else {
			vramSource = "unknown"
		}

		// Fetch downloaded models from /api/tags (cached 30s in router).
		tagModels, err := s.router.FetchModelTags(nodeURL)
		if err != nil {
			// Node unreachable  --  emit an empty entry so the UI still shows the node.
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

func safeModelInfoSlice(slice []router.ModelInfo) []router.ModelInfo {
	if slice == nil {
		return []router.ModelInfo{}
	}
	out := make([]router.ModelInfo, len(slice))
	copy(out, slice)
	return out
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	totalMB, freeMB := readSystemMemory()

	nodes := s.router.Nodes()
	gpus := make([]sysGPUEntry, len(nodes))
	for i, n := range nodes {
		n.RLock()
		var vramFreeMB int64
		if n.VRAMTotalMB > 0 {
			vramUsedMBFromPS := int64(0)
			for _, m := range n.LoadedModels {
				vramUsedMBFromPS += m.SizeVRAM / (1024 * 1024)
			}
			vramFreeMB = n.VRAMTotalMB - vramUsedMBFromPS
			if vramFreeMB < 0 {
				vramFreeMB = 0
			}
		}
		// Copy the underlying values under the read lock. Taking the address of
		// (or aliasing) a NodeState field would let the health-poll goroutine
		// mutate it while json.Encode dereferences it after RUnlock - a data race.
		var tempC *float64
		if n.Temperature != nil {
			v := *n.Temperature
			tempC = &v
		}
		var powerW *float64
		if n.PowerDrawW > 0 {
			v := n.PowerDrawW
			powerW = &v
		}
		gpus[i] = sysGPUEntry{
			Name:         n.Name,
			URL:          n.URL,
			VRAMTotalMB:  n.VRAMTotalMB,
			VRAMFreeMB:   vramFreeMB,
			VRAMSource:   n.VRAMSource,
			TemperatureC: tempC,
			PowerDrawW:   powerW,
			Healthy:      n.Healthy,
		}
		n.RUnlock()
	}

	tzName := s.router.Timezone()
	if tzName == "" {
		tzName = "Local"
	}

	nowTime := time.Now()
	if tzName != "Local" {
		loc, err := time.LoadLocation(tzName)
		if err == nil {
			nowTime = nowTime.In(loc)
		}
	}
	zone, _ := nowTime.Zone()

	displayTz := tzName
	if tzName == "Local" {
		displayTz = zone
	}

	info := SystemInfo{
		CPUCores:   runtime.NumCPU(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		RAMTotalMB: totalMB,
		RAMFreeMB:  freeMB,
		GPUs:       gpus,
		ServerTime: nowTime.Format("2006-01-02 15:04:05"),
		Timezone:   displayTz,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}
