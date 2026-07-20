package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

//go:embed web/dist
var webFS embed.FS

type ctxKey string

const ctxKeyUsername ctxKey = "username"

// sessionCookieName is the httpOnly cookie holding the admin session token.
// The token itself never reaches client-side JS or localStorage (Priority 2,
// 2026-07-14 audit) - only this cookie carries it, and only the server reads
// it back.
const sessionCookieName = "mesh_session"

// sessionTokenFromRequest reads the session token from the httpOnly cookie.
func sessionTokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		return c.Value
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// isRequestSecure reports whether the request should be treated as HTTPS for
// cookie Secure-flag purposes: either a direct TLS connection, or fronted by
// a reverse proxy that says so via X-Forwarded-Proto. The header is only
// meaningful behind a real reverse proxy - on a direct, un-proxied listener
// a client could set it itself, but the worst case is just Secure not being
// set when it ideally would be, not a new hole (the cookie is still
// HttpOnly either way).
func isRequestSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// setSessionCookie sets the httpOnly session cookie. expiry zero-value means
// a session cookie (cleared when the browser closes) rather than persistent.
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiry time.Time) {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isRequestSecure(r),
		SameSite: http.SameSiteLaxMode,
	}
	if !expiry.IsZero() {
		cookie.Expires = expiry
	}
	http.SetCookie(w, cookie)
}

// clearSessionCookie expires the session cookie immediately (logout).
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isRequestSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// writeJSONError writes a well-formed {"error": msg} JSON body with the given
// status code. Use this (rather than hand-building JSON with fmt.Sprintf and
// %q) whenever msg embeds a runtime value: %q already wraps its argument in
// its own literal quote characters, so splicing it into a template that is
// itself already inside a JSON string (e.g. `{"error":"node %q not found"}`)
// produces invalid JSON - the embedded quotes prematurely close the JSON
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

// newCorrelationID returns a short request-scoped ID for tying a generic
// client-facing error to the detailed server log line, without ever putting
// the real error text on the wire (2026-07-14 audit, Priority 4).
func newCorrelationID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("r%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// writeServerError logs the real error server-side with a correlation ID and
// writes only a generic message + that ID to the client (HTTP 500) - never
// err.Error(), which can leak DB/file/library internals (2026-07-14 audit,
// Priority 4). Use this for any failure that isn't pure request-shape
// validation (DB errors, marshal errors, hashing errors, etc).
func writeServerError(w http.ResponseWriter, r *http.Request, err error) {
	writeCorrelatedError(w, r, http.StatusInternalServerError, "internal error", err)
}

// writeCorrelatedError is writeServerError with a caller-chosen status, for
// non-500 failures (e.g. a 502 relaying an upstream/network error) that must
// still never echo err.Error() to the client.
func writeCorrelatedError(w http.ResponseWriter, r *http.Request, status int, publicMsg string, err error) {
	id := newCorrelationID()
	log.Printf("admin: request %s %s error (id=%s, status=%d): %v", r.Method, r.URL.Path, id, status, err)
	writeJSONError(w, status, fmt.Sprintf("%s (request id: %s)", publicMsg, id))
}

type TokenEvent struct {
	Time   time.Time
	Tokens int64
}

type Server struct {
	router         *router.Router
	auth           *auth.Middleware
	cfg            config.Config
	version        string
	mu             sync.RWMutex
	requests       []RequestLog
	localCount     int64   // atomic - requests served by local nodes
	cloudCount     int64   // atomic - requests forwarded to cloud
	localTokens    int64   // atomic - real token counts parsed from local node responses
	cloudTokens    int64   // atomic - real token counts parsed from cloud responses
	cloudSpentUSD  float64 // protected by mu
	refCostPer1K   float64 // reference cloud rate used to value local tokens (immutable after construction)
	startTime      time.Time
	analytics      *analyticsStore
	auditLog       *audit.Logger
	st             store.Store // never nil; NopStore when persistence disabled
	demoMode       bool        // when true, login accepts admin/admin without DB
	loginLimiter   *loginRateLimiter
	resetPwLimiter *loginRateLimiter // 3/hour per IP on admin-triggered password resets
	logChan        chan store.RequestRecord
	logDone        chan struct{} // closed by Shutdown to signal drain-and-stop
	logWg          sync.WaitGroup
	pullsMu        sync.Mutex                // guards pullJobs
	pullJobs       map[string]*pullJob       // "node|model" -> job state; ephemeral, never persisted
	coldStarts     int64                     // atomic - total cold start events
	warmHits       int64                     // atomic - total warm hit events
	tokenEvents    []TokenEvent              // protected by mu
	mgmtEndpoints  managementEndpointsSetter // nil until wired via SetProxyHandler
}

// managementEndpointsSetter is satisfied by *proxy.Handler. Defined locally
// (rather than importing internal/proxy) because proxy already imports
// admin to reach the Server for its own request handling.
type managementEndpointsSetter interface {
	SetAllowManagementEndpoints(bool)
	SetTrustProxyHeaders(bool)
}

// SetProxyHandler wires the proxy handler so routing.allow_management_endpoints
// changes made via handleUpdateSettings/handleConfigReload take effect
// immediately instead of only at boot.
func (s *Server) SetProxyHandler(p managementEndpointsSetter) { s.mgmtEndpoints = p }

// SetVersion sets the version string reported by /health.
// Call this from main with the ldflags-injected version before serving.
func (s *Server) SetVersion(v string) {
	s.version = v
}

// SetAuditLogger wires the audit logger into the admin server for query access.
func (s *Server) SetAuditLogger(al *audit.Logger) {
	s.auditLog = al
}

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
// Errors are non-fatal - the server still runs, just with a cold cache.
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

		// Seed node-level counters. These fields are otherwise only ever
		// touched via atomic.Add/LoadInt64 (see LogRequest), so seed them
		// the same way rather than mixing in a mutex-guarded plain write.
		for _, n := range s.router.Nodes() {
			atomic.StoreInt64(&n.ColdStarts, nodeCold[n.Name])
			atomic.StoreInt64(&n.WarmHits, nodeWarm[n.Name])
			atomic.StoreInt64(&n.LatencySumMs, nodeLatency[n.Name])
			atomic.StoreInt64(&n.LatencyCount, nodeLatencyCount[n.Name])
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
	Host             string             `json:"host"`
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
	// Node Agent-derived fields (internal/nodeagent). AgentPresent is false
	// (and every other field below zero-value) whenever no agent is
	// configured for this node, or the most recent agent poll failed - the
	// UI must check AgentPresent before displaying FanPercent/RAMUsedMB/
	// DiskFreeGB/AgentVersion, never treat a zero as a real measurement (R1).
	AgentPresent bool     `json:"agentPresent"`
	AgentVersion string   `json:"agentVersion,omitempty"`
	FanPercent   *float64 `json:"fanPercent"`
	CPUPercent   float64  `json:"cpuPercent"`
	RAMUsedMB    int64    `json:"ramUsedMB"`
	DiskFreeGB   float64  `json:"diskFreeGB"`
	// AgentCapabilities/AgentPlatform/AgentArchitecture/AgentGPUVendor/
	// AgentRuntime are the agent's self-reported metadata (see
	// internal/nodeagent Telemetry.Capabilities/Platform/Architecture/
	// GPUVendor/Runtime) - lets the UI gate agent-dependent features on
	// what this specific node's agent build actually supports, and helps
	// debug a mixed-version/mixed-vendor/mixed-runtime fleet. Cleared
	// alongside AgentPresent, same R1 discipline as the fields above.
	AgentCapabilities []string `json:"agentCapabilities,omitempty"`
	AgentPlatform     string   `json:"agentPlatform,omitempty"`
	AgentArchitecture string   `json:"agentArchitecture,omitempty"`
	AgentGPUVendor    string   `json:"agentGpuVendor,omitempty"`
	AgentRuntime      string   `json:"agentRuntime,omitempty"`
	// AgentNodeID is the agent's self-persisted node_id (a stable UUID
	// surviving agent upgrades/hostname changes - internal/nodeagent
	// identity.go). AgentGPUCount/AgentGPUs/DriverVersion/CUDAVersion are the
	// multi-GPU array + driver-stack metadata; RAMTotalMB/DiskTotalGB/
	// Hostname/UptimeSeconds/BootTime are host capacity/identity;
	// RuntimeVersion/RuntimeStatus are the detected runtime's own reported
	// version/live reachability. All cleared alongside AgentPresent, same R1
	// discipline as every other agent-derived field above.
	AgentNodeID    string           `json:"agentNodeId,omitempty"`
	AgentGPUCount  int              `json:"agentGpuCount,omitempty"`
	AgentGPUs      []agentGPUDevice `json:"agentGpus,omitempty"`
	DriverVersion  string           `json:"driverVersion,omitempty"`
	CUDAVersion    string           `json:"cudaVersion,omitempty"`
	RAMTotalMB     int64            `json:"ramTotalMB,omitempty"`
	DiskTotalGB    float64          `json:"diskTotalGB,omitempty"`
	Hostname       string           `json:"hostname,omitempty"`
	UptimeSeconds  int64            `json:"uptimeSeconds,omitempty"`
	BootTime       int64            `json:"bootTime,omitempty"`
	RuntimeVersion string           `json:"runtimeVersion,omitempty"`
	RuntimeStatus  string           `json:"runtimeStatus,omitempty"`
}

// agentGPUDevice is the admin API's camelCase projection of
// nodeagent.GPUInfo (whose own JSON tags are snake_case, matching the
// Node Agent Protocol wire format, not this admin API's convention) - one
// entry per physical GPU device in a node's multi-GPU array.
type agentGPUDevice struct {
	Index        int      `json:"index"`
	Vendor       string   `json:"vendor,omitempty"`
	CorePercent  *float64 `json:"corePercent,omitempty"`
	TemperatureC *float64 `json:"temperatureC,omitempty"`
	FanPercent   *float64 `json:"fanPercent,omitempty"`
	PowerWatts   *float64 `json:"powerWatts,omitempty"`
	VRAMUsedMB   int64    `json:"vramUsedMB,omitempty"`
	VRAMTotalMB  int64    `json:"vramTotalMB,omitempty"`
}

// toAgentGPUDevices converts the agent-protocol GPU array into the admin
// API's camelCase projection above. Returns nil (omitted via omitempty) for
// a nil/empty input, never an empty-but-present array.
func toAgentGPUDevices(devices []nodeagent.GPUInfo) []agentGPUDevice {
	if len(devices) == 0 {
		return nil
	}
	out := make([]agentGPUDevice, len(devices))
	for i, d := range devices {
		out[i] = agentGPUDevice{
			Index:        d.Index,
			Vendor:       d.Vendor,
			CorePercent:  d.CorePercent,
			TemperatureC: d.TemperatureC,
			FanPercent:   d.FanPercent,
			PowerWatts:   d.PowerWatts,
			VRAMUsedMB:   d.VRAMUsedMB,
			VRAMTotalMB:  d.VRAMTotalMB,
		}
	}
	return out
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
		router:         r,
		auth:           a,
		cfg:            cfg,
		version:        "dev",
		refCostPer1K:   refRate,
		startTime:      time.Now(),
		analytics:      newAnalyticsStore(refRate),
		st:             stImpl,
		loginLimiter:   newLoginRateLimiter(),
		resetPwLimiter: newResetPasswordRateLimiter(),
		logChan:        make(chan store.RequestRecord, 5000),
		logDone:        make(chan struct{}),
		pullJobs:       make(map[string]*pullJob),
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
// user sessions and audit_log rows past their retention window every 12
// hours (plus once at startup, so a long-idle mesh doesn't wait 12h for its
// first prune). Call once after construction; ctx cancellation stops the
// ticker, mirroring StartCounterFlush.
func (s *Server) StartPeriodicCleanup(ctx context.Context) {
	prune := func() {
		s.st.PruneExpiredUserSessions()
		s.mu.RLock()
		retentionDays := s.cfg.Audit.RetentionDays
		systemRetentionDays := s.cfg.Audit.SystemAuditRetentionDays
		s.mu.RUnlock()
		if err := s.st.PruneAuditLog(retentionDays); err != nil {
			log.Printf("admin: audit log prune failed: %v", err)
		}
		if err := s.st.PruneSystemAuditLog(systemRetentionDays); err != nil {
			log.Printf("admin: system audit log prune failed: %v", err)
		}
	}
	go func() {
		prune()
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
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
	reg("POST /admin/cloud/providers", s.cors(s.adminAuth(s.handleAddCloudProvider)))
	reg("PUT /admin/cloud/providers/{name}", s.cors(s.adminAuth(s.handleUpdateCloudProvider)))
	reg("DELETE /admin/cloud/providers/{name}", s.cors(s.adminAuth(s.handleDeleteCloudProvider)))
	reg("PUT /admin/cloud/providers/reorder", s.cors(s.adminAuth(s.handleReorderCloudProviders)))
	reg("POST /admin/cloud/providers/test", s.cors(s.adminAuth(s.handleTestCloudProvider)))
	reg("GET /admin/analytics", s.cors(s.adminAuth(s.handleAnalytics)))
	reg("GET /admin/analytics/export", s.cors(s.adminAuth(s.handleAnalyticsExport)))
	reg("GET /admin/models", s.cors(s.adminAuth(s.handleModels)))
	reg("POST /admin/nodes/{name}/pull", s.cors(s.adminAuth(s.handleNodePull)))
	reg("GET /admin/nodes/{name}/pull/progress", s.cors(s.adminAuth(s.handlePullProgress)))
	reg("DELETE /admin/nodes/{name}/pull", s.cors(s.adminAuth(s.handleCancelPull)))
	reg("POST /admin/nodes/{name}/drain", s.cors(s.adminAuth(s.handleDrainNode)))
	reg("DELETE /admin/nodes/{name}/drain", s.cors(s.adminAuth(s.handleUndrainNode)))
	reg("POST /admin/nodes/{name}/prewarm", s.cors(s.adminAuth(s.handleSetNodePrewarm)))
	reg("GET /admin/nodes/{name}/agent", s.cors(s.adminAuth(s.handleGetNodeAgent)))
	reg("POST /admin/nodes/{name}/agent", s.cors(s.adminAuth(s.handleEnableNodeAgent)))
	reg("DELETE /admin/nodes/{name}/agent", s.cors(s.adminAuth(s.handleDisableNodeAgent)))
	reg("POST /admin/nodes/{name}/agent/regenerate", s.cors(s.adminAuth(s.handleRegenerateNodeAgentToken)))
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

	reg("GET /admin/system-info", s.cors(s.adminAuth(s.handleSystemInfo)))
	reg("GET /admin/cloud-budget-status", s.cors(s.adminAuth(s.handleCloudBudgetStatus)))

	reg("GET /admin/warmup", s.cors(s.adminAuth(s.handleWarmupStatus)))
	reg("PUT /admin/warmup/predictive", s.cors(s.adminAuth(s.handleSetPredictiveEngine)))
	reg("POST /admin/warmup/ping", s.cors(s.adminAuth(s.handleWarmupPing)))

	reg("POST /admin/config/reload", s.cors(s.adminAuth(s.handleConfigReload)))

	// Health check and login - no auth required.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /login", s.cors(s.handleLogin))
	mux.HandleFunc("POST /admin/login", s.cors(s.handleAdminLogin))
	reg("POST /admin/logout", s.cors(s.adminAuth(s.handleLogout)))
	reg("POST /admin/change-password", s.cors(s.adminAuth(s.handleChangePassword)))
	reg("POST /admin/skip-password-change", s.cors(s.adminAuth(s.handleSkipPasswordChange)))
	// Role-agnostic endpoints - any valid session (admin or user).
	mux.HandleFunc("POST /change-password", s.cors(s.sessionAuth(s.handleChangePassword)))
	mux.HandleFunc("POST /skip-password-change", s.cors(s.sessionAuth(s.handleSkipPasswordChange)))
	mux.HandleFunc("POST /logout", s.cors(s.sessionAuth(s.handleLogout)))

	// User management (admin only, no /admin/* duplicate - these are v1-only)
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
		// Grafana dashboard JSON download - would otherwise fall through to the
		// SPA catch-all and return index.html instead of the dashboard.
		mux.Handle("/grafana/ollama-mesh.json", s.noCache(http.FileServer(http.FS(sub))))
	} else {
		fmt.Println("warn: failed to embed web UI:", err)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve index.html for SPA routes; block unknown /admin/* API paths.
		// /admin/login is a frontend SPA route, not an API path - let it through.
		// Block unknown /admin/* API paths; /admin/login is a SPA route.
		// /login and /change-password are API endpoints registered above - they
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
			// Required for the session cookie to be sent/read cross-origin at
			// all - safe to pair with a configured origin since it's never a
			// wildcard here (reflects exactly the one configured cors_origin).
			w.Header().Set("Access-Control-Allow-Credentials", "true")
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
		token := sessionTokenFromRequest(r)
		if token != "" {
			if session, found, err := s.st.GetUserSession(token); err == nil && found {
				if session.MustChangePassword {
					p := r.URL.Path
					allowed := p == "/change-password" ||
						p == "/admin/v1/change-password" || p == "/admin/change-password" ||
						p == "/skip-password-change" ||
						p == "/admin/v1/skip-password-change" || p == "/admin/skip-password-change" ||
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
		token := sessionTokenFromRequest(r)

		// Demo mode: accept the literal "demo-session" token so the GitHub Pages
		// demo works without a DB. Not a security concern - demo mode is opt-in,
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
				p == "/admin/v1/skip-password-change" || p == "/admin/skip-password-change" ||
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
		host := ""
		port := 0
		if u, err := url.Parse(n.URL); err == nil {
			host = u.Hostname()
			port, _ = strconv.Atoi(u.Port())
		}
		health := "healthy"
		if !n.Healthy {
			health = "down"
		} else if n.Failures > 0 {
			health = "degraded"
		}
		// Empty history stays empty ([] in JSON) - the UI renders a "no data" state.
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
			ID:                fmt.Sprintf("gpu-%d", i),
			Name:              n.Name,
			Host:              host,
			Port:              port,
			GPUModel:          n.GPUModel,
			VRAMTotalMB:       n.VRAMTotalMB,
			VRAMUsedMB:        n.VRAMUsedMB,
			VRAMSource:        n.VRAMSource,
			PowerDrawW:        n.PowerDrawW,
			Temperature:       n.Temperature,
			Runtime:           n.Runtime,
			Health:            health,
			Draining:          n.Draining,
			DrainedReason:     n.DrainedReason,
			PrewarmDisabled:   n.PrewarmDisabled,
			Uptime:            n.Uptime,
			LoadedModels:      safeModelInfoSlice(n.LoadedModels),
			ActiveConns:       atomic.LoadInt32(&n.ActiveConns),
			RequestsTotal:     atomic.LoadInt64(&n.RequestsTotal),
			HealthHistory:     hist,
			PendingPrewarmMB:  s.router.PendingPrewarmBytes(n.Name) / (1024 * 1024),
			ColdStarts:        coldNode,
			TokensTotal:       atomic.LoadInt64(&n.TokensTotal),
			AvgLatencyMs:      avgLatencyNode,
			WarmHitRatio:      warmHitRatioNode,
			AgentPresent:      n.AgentPresent,
			AgentVersion:      n.AgentVersion,
			FanPercent:        n.FanPercent,
			CPUPercent:        n.CPUPercent,
			RAMUsedMB:         n.RAMUsedMB,
			DiskFreeGB:        n.DiskFreeGB,
			AgentCapabilities: n.AgentCapabilities,
			AgentPlatform:     n.AgentPlatform,
			AgentArchitecture: n.AgentArchitecture,
			AgentGPUVendor:    n.AgentGPUVendor,
			AgentRuntime:      n.AgentRuntime,
			AgentNodeID:       n.AgentNodeID,
			AgentGPUCount:     n.AgentGPUCount,
			AgentGPUs:         toAgentGPUDevices(n.AgentGPUs),
			DriverVersion:     n.DriverVersion,
			CUDAVersion:       n.CUDAVersion,
			RAMTotalMB:        n.RAMTotalMB,
			DiskTotalGB:       n.DiskTotalGB,
			Hostname:          n.Hostname,
			UptimeSeconds:     n.UptimeSeconds,
			BootTime:          n.BootTime,
			RuntimeVersion:    n.RuntimeVersion,
			RuntimeStatus:     n.RuntimeStatus,
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
		host := ""
		port := 0
		if u, err := url.Parse(n.URL); err == nil {
			host = u.Hostname()
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
			ID:                fmt.Sprintf("gpu-%d", i),
			Name:              n.Name,
			Host:              host,
			Port:              port,
			GPUModel:          n.GPUModel,
			VRAMTotalMB:       n.VRAMTotalMB,
			VRAMUsedMB:        n.VRAMUsedMB,
			VRAMSource:        n.VRAMSource,
			PowerDrawW:        n.PowerDrawW,
			Temperature:       n.Temperature,
			Runtime:           n.Runtime,
			Health:            health,
			Draining:          n.Draining,
			DrainedReason:     n.DrainedReason,
			PrewarmDisabled:   n.PrewarmDisabled,
			Uptime:            n.Uptime,
			LoadedModels:      safeModelInfoSlice(n.LoadedModels),
			ActiveConns:       atomic.LoadInt32(&n.ActiveConns),
			HealthHistory:     hist,
			PendingPrewarmMB:  s.router.PendingPrewarmBytes(n.Name) / (1024 * 1024),
			AgentPresent:      n.AgentPresent,
			AgentVersion:      n.AgentVersion,
			FanPercent:        n.FanPercent,
			CPUPercent:        n.CPUPercent,
			RAMUsedMB:         n.RAMUsedMB,
			DiskFreeGB:        n.DiskFreeGB,
			AgentCapabilities: n.AgentCapabilities,
			AgentPlatform:     n.AgentPlatform,
			AgentArchitecture: n.AgentArchitecture,
			AgentGPUVendor:    n.AgentGPUVendor,
			AgentRuntime:      n.AgentRuntime,
			AgentNodeID:       n.AgentNodeID,
			AgentGPUCount:     n.AgentGPUCount,
			AgentGPUs:         toAgentGPUDevices(n.AgentGPUs),
			DriverVersion:     n.DriverVersion,
			CUDAVersion:       n.CUDAVersion,
			RAMTotalMB:        n.RAMTotalMB,
			DiskTotalGB:       n.DiskTotalGB,
			Hostname:          n.Hostname,
			UptimeSeconds:     n.UptimeSeconds,
			BootTime:          n.BootTime,
			RuntimeVersion:    n.RuntimeVersion,
			RuntimeStatus:     n.RuntimeStatus,
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
			if cfgNames[rk.Name] {
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
		writeServerError(w, r, err)
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

// ReloadFromStore re-syncs live router/auth state from SQLite - the
// mesh.db-first replacement for the old "reload config.yaml from disk"
// behavior. Used by both handleConfigReload (POST /admin/config/reload) and
// main.go's SIGHUP handler, since there is no file left to re-read; SQLite
// (already the source of truth for nodes/keys/cloud providers/settings) is
// re-applied to the live router/auth instead.
func (s *Server) ReloadFromStore() (nodesAdded, nodesRemoved, authKeys, cloudProviders int, err error) {
	runtimeKeys, kErr := s.st.AllKeys()
	if kErr != nil {
		return 0, 0, 0, 0, fmt.Errorf("load keys: %w", kErr)
	}
	keys := make([]config.KeyConfig, 0, len(runtimeKeys))
	for _, k := range runtimeKeys {
		if k.Revoked {
			continue
		}
		keys = append(keys, config.KeyConfig{
			Name:         k.Name,
			Key:          k.Key,
			RateLimit:    k.RateLimit,
			DailyLimit:   k.DailyLimit,
			MonthlyLimit: k.MonthlyLimit,
			Models:       k.Models,
		})
	}
	s.mu.RLock()
	authEnabled := s.cfg.Auth.Enabled
	s.mu.RUnlock()
	s.auth.Reload(config.AuthConfig{Enabled: authEnabled, Keys: keys})

	providers, cErr := s.st.AllCloudProviders()
	if cErr != nil {
		return 0, 0, 0, 0, fmt.Errorf("load cloud providers: %w", cErr)
	}
	cloudCfgs := make([]config.CloudProvider, len(providers))
	for i, p := range providers {
		cloudCfgs[i] = config.CloudProvider{
			Name: p.Name, Provider: p.Provider, BaseURL: p.BaseURL,
			APIKey: p.APIKey, DefaultModel: p.DefaultModel,
			CostPer1KTokens: p.CostPer1KTokens, Enabled: p.Enabled,
		}
	}
	s.router.SetClouds(cloudCfgs)

	runtimeNodes, nErr := s.st.AllNodes()
	if nErr != nil {
		return 0, 0, 0, 0, fmt.Errorf("load nodes: %w", nErr)
	}
	nodeCfgs := make([]config.NodeConfig, len(runtimeNodes))
	for i, n := range runtimeNodes {
		vram := int64(0)
		if n.VRAMTotalMB != nil {
			vram = *n.VRAMTotalMB
		}
		rt := n.Runtime
		if rt == "" {
			rt = "ollama"
		}
		nodeCfgs[i] = config.NodeConfig{Name: n.Name, URL: n.URL, Runtime: rt, VRAMTotalMB: vram}
	}
	added, removed := s.router.SyncNodes(nodeCfgs)

	s.mu.Lock()
	s.cfg.Auth.Keys = keys
	s.cfg.CloudProviders = cloudCfgs
	s.cfg.Nodes = nodeCfgs
	s.mu.Unlock()

	return added, removed, len(keys), len(cloudCfgs), nil
}

// handleConfigReload re-syncs live state from SQLite without restarting.
// POST /admin/config/reload (also /admin/v1/config/reload) - useful in
// container environments where sending SIGHUP is inconvenient (Kubernetes,
// Nomad, etc.).
func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	added, removed, authKeys, cloudProviders, err := s.ReloadFromStore()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	log.Printf("config reloaded via API from store (auth keys: %d, nodes: +%d/-%d, cloud providers: %d)",
		authKeys, added, removed, cloudProviders)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"reloaded":true,"auth_keys":%d,"nodes_added":%d,"nodes_removed":%d,"cloud_providers":%d}`,
		authKeys, added, removed, cloudProviders)
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
	if !isValidRuntime(cfg.Runtime) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown runtime %q (valid: ollama, vllm, tgi, llamacpp, mlx, auto)", cfg.Runtime))
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
	_ = s.st.DeleteNode(name)                    // cascades node_agent deletion, see sqliteStore.DeleteNode
	_ = s.st.SetSetting("warmup:node:"+name, "") // drop any warmup setting for the node
	s.logSystemChange(r, "remove_node", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// nodeAgentInstallCommand returns the one-line commands an operator runs on
// the GPU node to download the binary (if not already present) AND register
// it as a persistent, auto-restarting OS service - install.sh/install.ps1's
// ROLE=agent path (see .local/specs/node-agent.md section 12), which
// downloads the binary then hands off to its own "ollama-mesh agent service
// install" self-registration subcommand (internal/nodeagent/service). unix
// covers Linux/macOS; windows is the PowerShell equivalent for Windows
// nodes, since a POSIX sh script can't run there. Safe to re-run for an
// upgrade or to rotate the token - install.sh/service install are both
// idempotent.
func nodeAgentInstallCommand(port int, token string) (unix string, windows string) {
	unix = fmt.Sprintf(
		"curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | ROLE=agent TOKEN=%s PORT=%d sh",
		token, port,
	)
	windows = fmt.Sprintf(
		`$env:ROLE="agent"; $env:TOKEN="%s"; $env:PORT="%d"; irm https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.ps1 | iex`,
		token, port,
	)
	return unix, windows
}

// generateNodeAgentToken returns a 32-random-byte, base64url-encoded opaque
// token (per .local/specs/node-agent.md section 5 - a distinct protocol
// from the client-facing API-key mechanism, not a reuse of it).
func generateNodeAgentToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// handleGetNodeAgent returns the current Node Agent configuration for a
// node, without the token (the token is only ever returned by the
// enable/regenerate endpoints, at the moment an operator needs to copy it
// into the install command).
// GET /admin/nodes/{name}/agent
func (s *Server) handleGetNodeAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, found := s.router.NodeURLs()[name]; !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	rec, found, err := s.st.GetNodeAgent(name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to read node agent config")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !found {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"node": name, "enabled": false, "port": 0,
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"node": name, "enabled": rec.Enabled, "port": rec.Port,
	})
}

// handleEnableNodeAgent enables (or reconfigures) the Node Agent for a node:
// generates a fresh token, persists {enabled, port, token}, pushes the
// config to the live router so polling starts on the next cycle without a
// restart, and returns the one-line install command with the token
// embedded - the only response that ever carries the plaintext token.
// POST /admin/nodes/{name}/agent  body: {"port": <int>}
func (s *Server) handleEnableNodeAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, found := s.router.NodeURLs()[name]; !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	var body struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Port <= 0 || body.Port > 65535 {
		writeJSONError(w, http.StatusBadRequest, "port must be between 1 and 65535")
		return
	}
	token, err := generateNodeAgentToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	rec := store.NodeAgentRecord{Name: name, Enabled: true, Port: body.Port, Token: token}
	if err := s.st.UpsertNodeAgent(rec); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to persist node agent config")
		return
	}
	s.router.SetNodeAgent(name, true, body.Port, token)
	s.logSystemChange(r, "enable_node_agent", name, fmt.Sprintf("Port: %d", body.Port))
	unixCmd, windowsCmd := nodeAgentInstallCommand(body.Port, token)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"node":                    name,
		"enabled":                 true,
		"port":                    body.Port,
		"token":                   token,
		"install_command":         unixCmd,
		"install_command_windows": windowsCmd,
	})
}

// handleDisableNodeAgent disables and deletes the Node Agent config for a
// node - the router stops polling it on the next cycle (pollAgentTelemetry's
// "no agent configured" branch clears any previously-reported fields).
// DELETE /admin/nodes/{name}/agent
func (s *Server) handleDisableNodeAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, found := s.router.NodeURLs()[name]; !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	if err := s.st.DeleteNodeAgent(name); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to delete node agent config")
		return
	}
	s.router.SetNodeAgent(name, false, 0, "")
	s.logSystemChange(r, "disable_node_agent", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleRegenerateNodeAgentToken issues a fresh token for an already-enabled
// node agent, keeping its configured port. Returns 404 if the agent isn't
// currently enabled for this node (regenerating a token for a disabled/
// nonexistent agent has no meaning - use handleEnableNodeAgent instead).
// POST /admin/nodes/{name}/agent/regenerate
func (s *Server) handleRegenerateNodeAgentToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rec, found, err := s.st.GetNodeAgent(name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to read node agent config")
		return
	}
	if !found || !rec.Enabled {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node agent not enabled for %q", name))
		return
	}
	token, err := generateNodeAgentToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	rec.Token = token
	if err := s.st.UpsertNodeAgent(rec); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to persist node agent config")
		return
	}
	s.router.SetNodeAgent(name, true, rec.Port, token)
	s.logSystemChange(r, "regenerate_node_agent_token", name, "")
	unixCmd, windowsCmd := nodeAgentInstallCommand(rec.Port, token)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"node":                    name,
		"port":                    rec.Port,
		"token":                   token,
		"install_command":         unixCmd,
		"install_command_windows": windowsCmd,
	})
}

// syncCloudProvidersToRouter reloads every persisted cloud provider from the
// store and pushes the full list to the live router, so add/update/delete
// take effect immediately (same pattern as node add/remove).
func (s *Server) syncCloudProvidersToRouter() ([]config.CloudProvider, error) {
	providers, err := s.st.AllCloudProviders()
	if err != nil {
		return nil, err
	}
	clouds := make([]config.CloudProvider, len(providers))
	for i, p := range providers {
		clouds[i] = config.CloudProvider{
			Name: p.Name, Provider: p.Provider, BaseURL: p.BaseURL,
			APIKey: p.APIKey, DefaultModel: p.DefaultModel,
			CostPer1KTokens: p.CostPer1KTokens, Enabled: p.Enabled,
			Priority: p.Priority,
		}
	}
	s.router.SetClouds(clouds)
	s.mu.Lock()
	s.cfg.CloudProviders = clouds
	s.mu.Unlock()
	return clouds, nil
}

// handleAddCloudProvider adds (or, on name collision, replaces) a cloud
// fallback provider. POST /admin/cloud/providers.
func (s *Server) handleAddCloudProvider(w http.ResponseWriter, r *http.Request) {
	var cp store.CloudProviderRecord
	if err := json.NewDecoder(r.Body).Decode(&cp); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if cp.Name == "" || cp.Provider == "" {
		writeJSONError(w, http.StatusBadRequest, "name and provider are required")
		return
	}
	if cp.Enabled {
		if cp.BaseURL == "" || cp.APIKey == "" {
			writeJSONError(w, http.StatusBadRequest, "base_url and api_key are required when enabled")
			return
		}
		if err := config.ValidateNodeURL(cp.BaseURL); err != nil {
			writeJSONError(w, http.StatusBadRequest, "base_url must be http(s) with a host")
			return
		}
	}
	if err := s.st.UpsertCloudProvider(cp); err != nil {
		writeServerError(w, r, err)
		return
	}
	if _, err := s.syncCloudProvidersToRouter(); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.logSystemChange(r, "add_cloud_provider", cp.Name, fmt.Sprintf("Provider: %s, Enabled: %v", cp.Provider, cp.Enabled))
	w.WriteHeader(http.StatusCreated)
}

// handleUpdateCloudProvider updates an existing cloud provider by name.
// PUT /admin/cloud/providers/{name}.
func (s *Server) handleUpdateCloudProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	var cp store.CloudProviderRecord
	if err := json.NewDecoder(r.Body).Decode(&cp); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cp.Name = name
	// The client echoes back the masked "***" placeholder when the operator
	// didn't change the key; preserve the real value instead of clobbering it.
	if cp.APIKey == "" || cp.APIKey == "***" {
		existing, err := s.st.AllCloudProviders()
		if err == nil {
			for _, p := range existing {
				if p.Name == name {
					cp.APIKey = p.APIKey
					break
				}
			}
		}
	}
	if cp.Enabled {
		if cp.BaseURL == "" || cp.APIKey == "" {
			writeJSONError(w, http.StatusBadRequest, "base_url and api_key are required when enabled")
			return
		}
		if err := config.ValidateNodeURL(cp.BaseURL); err != nil {
			writeJSONError(w, http.StatusBadRequest, "base_url must be http(s) with a host")
			return
		}
	}
	if err := s.st.UpsertCloudProvider(cp); err != nil {
		writeServerError(w, r, err)
		return
	}
	if _, err := s.syncCloudProvidersToRouter(); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.logSystemChange(r, "update_cloud_provider", name, fmt.Sprintf("Provider: %s, Enabled: %v", cp.Provider, cp.Enabled))
	w.WriteHeader(http.StatusOK)
}

// cloudProviderTestTimeout bounds how long handleTestCloudProvider waits for
// the upstream provider to answer before reporting the key as unreachable.
const cloudProviderTestTimeout = 8 * time.Second

// handleTestCloudProvider verifies a base_url + api_key pair actually
// authenticates against the provider before it gets saved. Most configured
// providers speak the OpenAI-compatible surface (see proxyToCloud) and gate
// GET /models on the Authorization header, so that's the default probe.
// Two providers need a different probe because their /models (or equivalent)
// does not faithfully gate on the key:
//   - anthropic authenticates via "x-api-key" + "anthropic-version", not
//     "Authorization: Bearer" - the generic probe always 401s a valid key.
//   - openrouter's GET /v1/models is public and returns 200 for any key
//     (even garbage), so it must use GET /api/v1/auth/key instead, which
//     404/401s on a bad key.
//
// POST /admin/cloud/providers/test.
func (s *Server) handleTestCloudProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.BaseURL == "" || body.APIKey == "" {
		writeJSONError(w, http.StatusBadRequest, "base_url and api_key are required")
		return
	}
	if err := config.ValidateNodeURL(body.BaseURL); err != nil {
		writeJSONError(w, http.StatusBadRequest, "base_url must be http(s) with a host")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), cloudProviderTestTimeout)
	defer cancel()

	testURL := strings.TrimSuffix(body.BaseURL, "/") + "/models"
	if body.Provider == "openrouter" {
		testURL = strings.TrimSuffix(body.BaseURL, "/") + "/auth/key"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if body.Provider == "anthropic" {
		req.Header.Set("x-api-key", body.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+body.APIKey)
	}

	client := &http.Client{Timeout: cloudProviderTestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "could not reach base_url: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Never return 401 here: apiFetch (ui/src/lib/api.ts) treats ANY 401
		// from ANY admin endpoint as "the admin session expired" and force
		// logs the operator out. This 401 would be the *cloud provider's*
		// rejection of the key under test, not this admin session's - 400
		// keeps it a plain validation error instead of triggering a logout.
		writeJSONError(w, http.StatusBadRequest, "provider rejected the API key")
		return
	}
	if resp.StatusCode >= 400 {
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("provider returned %d", resp.StatusCode))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"ok":true}`)
}

// handleDeleteCloudProvider removes a cloud provider by name.
// DELETE /admin/cloud/providers/{name}.
func (s *Server) handleDeleteCloudProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.st.DeleteCloudProvider(name); err != nil {
		writeServerError(w, r, err)
		return
	}
	if _, err := s.syncCloudProvidersToRouter(); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.logSystemChange(r, "delete_cloud_provider", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleReorderCloudProviders takes the caller's desired display/attempt
// order (highest priority first) and renumbers every named provider's
// priority to match, then re-syncs the router so the new order takes effect
// immediately - same immediate-effect pattern as add/update/delete.
// PUT /admin/cloud/providers/reorder.
func (s *Server) handleReorderCloudProviders(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.Order) == 0 {
		writeJSONError(w, http.StatusBadRequest, "order is required")
		return
	}
	if err := s.st.SetCloudProviderPriorities(body.Order); err != nil {
		writeServerError(w, r, err)
		return
	}
	if _, err := s.syncCloudProvidersToRouter(); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.logSystemChange(r, "reorder_cloud_providers", "", fmt.Sprintf("Order: %v", body.Order))
	w.WriteHeader(http.StatusOK)
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
// waiting for LRU pressure - the manual counterpart to auto-eviction.
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
		writeCorrelatedError(w, r, http.StatusBadGateway, "failed to unload model on node", err)
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
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

// isValidRuntime reports whether runtime is a recognized node runtime value.
// Shared by handleAddNode and handlePatchNode so both reject the same set.
func isValidRuntime(runtime string) bool {
	switch runtime {
	case "ollama", "vllm", "tgi", "llamacpp", "mlx", "auto":
		return true
	default:
		return false
	}
}

// handlePatchNode applies runtime metadata overrides to a node.
// PATCH /admin/nodes/{name} - accepts {"vram_total_mb":N,"gpu_model":"...","runtime":"..."}
func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var patch router.NodePatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if patch.Runtime != nil && !isValidRuntime(*patch.Runtime) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown runtime %q (valid: ollama, vllm, tgi, llamacpp, mlx, auto)", *patch.Runtime))
		return
	}
	if patch.URL != nil {
		if err := s.router.UpdateNodeURL(name, *patch.URL); err != nil {
			status := http.StatusConflict
			if strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			} else if strings.Contains(err.Error(), "invalid URL") || strings.Contains(err.Error(), "must be http") || strings.Contains(err.Error(), "link-local") {
				status = http.StatusBadRequest
			}
			writeJSONError(w, status, err.Error())
			return
		}
		_ = s.st.UpdateNodeURL(name, *patch.URL)
	}
	if patch.VRAMTotalMB != nil || patch.GPUModel != nil || patch.Runtime != nil {
		if !s.router.PatchNode(name, patch) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
			return
		}
		_ = s.st.UpsertNodeOverride(name, patch.VRAMTotalMB, patch.GPUModel, patch.Runtime)
	}
	s.logSystemChange(r, "patch_node", name, fmt.Sprintf("URLChanged: %v, VRAMTotalMBChanged: %v, GPUModelChanged: %v, RuntimeChanged: %v", patch.URL != nil, patch.VRAMTotalMB != nil, patch.GPUModel != nil, patch.Runtime != nil))
	// Return the updated node.
	s.handleNode(w, r)
}

// handleGetModelConfig returns the configured default parameter profile for
// a model on a specific node. GET /admin/model-config?model=X&node=Y. 404 if
// no profile is configured for that exact pair - the caller sees the
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
		writeServerError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

// handleModelConfigCapabilities returns, for every known runtime, which
// ModelConfig fields actually take effect when injected - the single source
// of truth (store.SupportedFieldsFor) the UI reads to show only fields that
// are real for a given model/node's runtime, instead of hand-duplicating
// this list in TypeScript (which is exactly what caused this list to drift
// out of sync with the backend before). GET /admin/model-config/capabilities.
func (s *Server) handleModelConfigCapabilities(w http.ResponseWriter, r *http.Request) {
	out := map[string][]string{}
	for _, runtime := range []string{"ollama", "vllm", "tgi", "llamacpp", "mlx"} {
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
		writeServerError(w, r, err)
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
		writeServerError(w, r, err)
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
		writeServerError(w, r, err)
		return
	}
	s.logSystemChange(r, "delete_model_config", model+"@"+node, "reset model configuration profile to defaults")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"model": model, "node": node, "status": "reset"})
}

// generateAPIKey creates a cryptographically random API key of the form sk-<slug>-<48 hex chars>.
// name is slugified (lowercased, non-alphanumerics collapsed to a single hyphen) so the token
// stays a single whitespace-free string regardless of how the key's display name was entered.
func generateAPIKey(name string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	slug := apiKeyNameSlugRe.ReplaceAllString(strings.ToLower(name), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "key"
	}
	return "sk-" + slug + "-" + hex.EncodeToString(b)
}

var apiKeyNameSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// loginRateLimiter throttles admin login attempts per client IP to defend
// against brute-force credential guessing on the admin dashboard (port 8080).
// State is in-memory only (matches the rest of admin.go's in-process patterns)
// and resets on process restart - an acceptable tradeoff since a meaningful
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

// newRateLimiter builds an IP-keyed sliding-window limiter with the given
// parameters - shared constructor for both the login and reset-password
// limiters below (2026-07-14 audit, Priority 5).
func newRateLimiter(maxAttempts int, window, lockDuration time.Duration) *loginRateLimiter {
	return &loginRateLimiter{
		attempts:     make(map[string]*loginAttemptState),
		maxAttempts:  maxAttempts,
		window:       window,
		lockDuration: lockDuration,
	}
}

// newLoginRateLimiter: 5 attempts per minute per IP on login.
func newLoginRateLimiter() *loginRateLimiter {
	return newRateLimiter(5, time.Minute, 15*time.Minute)
}

// newResetPasswordRateLimiter: 3 attempts per hour per IP on password reset.
func newResetPasswordRateLimiter() *loginRateLimiter {
	return newRateLimiter(3, time.Hour, time.Hour)
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
	// Lockout expired (or none active) - if the failure window has also
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
// write lock). A dropped entry is harmless - the next failure just re-creates a
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

// handleAdminLogin handles POST /admin/login - admin role required.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	s.handleLoginForRole(w, r, "admin")
}

// handleLogin handles POST /login - any active role accepted.
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
			setSessionCookie(w, r, "demo-session", expiry)
			json.NewEncoder(w).Encode(map[string]interface{}{
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
		writeServerError(w, r, err)
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
		writeServerError(w, r, err)
		return
	}
	go s.st.PruneExpiredUserSessions()
	setSessionCookie(w, r, sessionToken, expiry)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"role":                 user.Role,
		"username":             user.Username,
		"must_change_password": user.MustChangePassword,
		"expires_at":           expiry.Format(time.RFC3339),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := sessionTokenFromRequest(r)
	_ = s.st.DeleteUserSession(token)
	_ = s.st.DeleteSession(token) // backward compat: also clear old admin_sessions
	clearSessionCookie(w, r)
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
	user.SkipPasswordCount = 0
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
	setSessionCookie(w, r, newToken, expiry)
	json.NewEncoder(w).Encode(map[string]string{
		"expires_at": expiry.Format(time.RFC3339),
	})
}

// maxSkipPasswordChanges caps how many times the forced-password-change
// screen can be dismissed (Grafana-style "Skip for now") before the account
// must actually change its password. Without a cap, an admin/admin install
// could stay on the public default password indefinitely - the cap forces
// resolution while still allowing a few "not right now" dismissals.
const maxSkipPasswordChanges = 3

// handleSkipPasswordChange lets an admin dismiss the forced-password-change
// screen for this session only, without touching the user's
// MustChangePassword flag in the users table - so the next fresh login
// still forces the prompt again. Each dismissal increments a persistent
// per-user counter; once maxSkipPasswordChanges is reached, skipping is
// refused and the caller must actually change the password. Reachable only
// via the same must-change-password bypass list as change-password/logout.
func (s *Server) handleSkipPasswordChange(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	username, _ := r.Context().Value(ctxKeyUsername).(string)
	user, err := s.st.GetUserByUsername(username)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"user not found"}`))
		return
	}
	if user.SkipPasswordCount >= maxSkipPasswordChanges {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"skip_limit_reached","message":"password must be changed - skip limit reached"}`))
		return
	}
	user.SkipPasswordCount++
	if err := s.st.UpdateUser(user); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"could not update user"}`))
		return
	}
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
	setSessionCookie(w, r, newToken, expiry)
	json.NewEncoder(w).Encode(map[string]string{
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid JSON"}`))
		return
	}

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
	if user.APIKeyName != "" {
		if s.auth != nil {
			s.auth.RevokeKey(user.APIKeyName)
		}
		_ = s.st.RevokeKey(user.APIKeyName)
	}
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
	if user.APIKeyName != "" {
		if s.auth != nil {
			s.auth.RevokeKey(user.APIKeyName)
		}
		_ = s.st.RevokeKey(user.APIKeyName)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid JSON"}`))
		return
	}

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
	if s.resetPwLimiter != nil {
		ip := clientIP(r)
		if ok, retryAfter := s.resetPwLimiter.allow(ip); !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
			writeJSONError(w, http.StatusTooManyRequests, "too many password resets, try again later")
			return
		}
		s.resetPwLimiter.recordFailure(ip)
	}
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
// The salt is embedded in the returned hash - no separate salt parameter needed.
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

// defaultAdminPassword is the well-known first-run admin password (same
// pattern as Grafana's default admin/admin). It is never logged or printed -
// it's public knowledge, documented in the startup banner and README, and
// MustChangePassword forces a change (or an explicit skip) before the
// account can be used for anything beyond the change-password endpoint.
const defaultAdminPassword = "admin"

// ensureAdminUser sets up the initial admin in the users table on first run.
// If the legacy admin_credentials table has data, it migrates that row.
// On a completely fresh install it creates admin/admin and forces a
// password change on first login - no secret is generated or logged.
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
	// Fresh install: well-known default, force change on first login.
	hash, hashErr := hashPassword(defaultAdminPassword)
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
	log.Printf("admin: created default admin account (username: admin); password must be changed on first login")
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

// validateExpiresAt parses an optional key expiry string and rejects malformed
// or already-past values. An empty string (no expiry) is always valid. Accepts
// a bare date, the datetime-local format the UI's date/time picker emits, or
// RFC3339, in that order.
func validateExpiresAt(s string) error {
	if s == "" {
		return nil
	}
	var exp time.Time
	var err error
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04", time.RFC3339} {
		if exp, err = time.Parse(layout, s); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("expires_at must be YYYY-MM-DD, YYYY-MM-DDTHH:MM, or RFC3339 format")
	}
	// Reject an already-past expiry: it would mint/patch a key that can never
	// authenticate (keyExpired treats it as expired immediately), which is a
	// silent footgun rather than an intended action.
	if !exp.After(time.Now()) {
		return fmt.Errorf("expires_at is in the past")
	}
	return nil
}

func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
	var k config.KeyConfig
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
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
	if err := validateExpiresAt(k.ExpiresAt); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
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
		ExpiresAt:     k.ExpiresAt,
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
	if patch.ExpiresAt != nil {
		if err := validateExpiresAt(*patch.ExpiresAt); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
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
		if patch.ExpiresAt != nil {
			keyRecord.ExpiresAt = *patch.ExpiresAt
		}
		_ = s.st.UpsertKey(*keyRecord)
	}
	s.logSystemChange(r, "patch_key", name, fmt.Sprintf("RateLimitChanged: %v, DailyLimitChanged: %v, MonthlyLimitChanged: %v, DailyUsdCapChanged: %v, MonthlyUsdCapChanged: %v, ModelsChanged: %v, ExpiresAtChanged: %v", patch.RateLimit != nil, patch.DailyLimit != nil, patch.MonthlyLimit != nil, patch.DailyUsdCap != nil, patch.MonthlyUsdCap != nil, patch.Models != nil, patch.ExpiresAt != nil))
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
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
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
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.router.SetStrategy(req.Strategy); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
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
	if cfg.HuggingFace.Token != "" {
		cfg.HuggingFace.Token = "***"
	}
	if cfg.Webhook.Secret != "" {
		cfg.Webhook.Secret = "***"
	}
	if cfg.LiteLLM.APIKey != "" {
		cfg.LiteLLM.APIKey = "***"
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
	s.mu.Lock()
	// Start from the current config so any field the request body omits (the
	// Settings page only ever sends a partial payload - most Routing/Auth/
	// CloudProviders fields are managed via their own dedicated endpoints)
	// keeps its existing value instead of being silently zeroed. Decoding
	// JSON onto a populated struct only overwrites keys actually present in
	// the body, which also makes explicit "false"/zero values (e.g.
	// disabling routing.session_affinity) apply correctly instead of being
	// mistaken for "unset".
	incoming := s.cfg
	// Auth.Enabled and Proxy.AccessLog are *bool: deep-copy them so decoding
	// into incoming can't mutate the value s.cfg's pointer still points to
	// if validation below fails and the update is discarded.
	if s.cfg.Auth.Enabled != nil {
		v := *s.cfg.Auth.Enabled
		incoming.Auth.Enabled = &v
	}
	if s.cfg.Proxy.AccessLog != nil {
		v := *s.cfg.Proxy.AccessLog
		incoming.Proxy.AccessLog = &v
	}
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// The client echoes back the masked "***" placeholder (or omits the field
	// entirely) when the operator didn't change it; preserve the real token in
	// both cases instead of clobbering it with the mask.
	if incoming.HuggingFace.Token == "" || incoming.HuggingFace.Token == "***" {
		incoming.HuggingFace.Token = s.cfg.HuggingFace.Token
	}
	if incoming.Webhook.Secret == "" || incoming.Webhook.Secret == "***" {
		incoming.Webhook.Secret = s.cfg.Webhook.Secret
	}
	if incoming.LiteLLM.APIKey == "" || incoming.LiteLLM.APIKey == "***" {
		incoming.LiteLLM.APIKey = s.cfg.LiteLLM.APIKey
	}

	if err := incoming.Validate(); err != nil {
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("validation failed: %v", err), http.StatusBadRequest)
		return
	}

	s.cfg = incoming
	s.mu.Unlock()

	if s.mgmtEndpoints != nil {
		s.mgmtEndpoints.SetAllowManagementEndpoints(incoming.Routing.AllowManagementEndpoints)
		s.mgmtEndpoints.SetTrustProxyHeaders(incoming.Proxy.TrustProxyHeaders)
	}
	s.router.SetTimezone(incoming.Timezone)
	s.router.SetLiteLLM(incoming.LiteLLM)
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
		"proxy_port":                strconv.Itoa(incoming.Proxy.Port),
		"proxy_log_format":          incoming.Proxy.LogFormat,
		"proxy_access_log":          strconv.FormatBool(incoming.Proxy.AccessLog == nil || *incoming.Proxy.AccessLog),
		"proxy_trust_proxy_headers": strconv.FormatBool(incoming.Proxy.TrustProxyHeaders),
		"litellm_url":               incoming.LiteLLM.URL,
		"litellm_enabled":           strconv.FormatBool(incoming.LiteLLM.Enabled),
		"litellm_api_key":           incoming.LiteLLM.APIKey,
		"cloud_daily_usd_cap":       strconv.FormatFloat(incoming.CloudBudget.DailyUSDCap, 'f', -1, 64),
		"cloud_monthly_usd_cap":     strconv.FormatFloat(incoming.CloudBudget.MonthlyUSDCap, 'f', -1, 64),
		"metrics_enabled":           strconv.FormatBool(incoming.Metrics.Enabled),
		"metrics_port":              strconv.Itoa(incoming.Metrics.Port),
		"huggingface_token":         incoming.HuggingFace.Token,

		// Admin & Security (2026-07 config.yaml elimination - Phase 2).
		"admin_bind_address": incoming.Admin.BindAddress,
		"admin_cors_origin":  incoming.Admin.CORSOrigin,

		// Advanced routing.
		"routing_fallback":                              incoming.Routing.Fallback,
		"routing_upstream_timeout_ms":                   strconv.Itoa(incoming.Routing.UpstreamTimeoutMs),
		"routing_max_retries":                           strconv.Itoa(incoming.Routing.MaxRetries),
		"routing_allow_management_endpoints":            strconv.FormatBool(incoming.Routing.AllowManagementEndpoints),
		"routing_session_affinity":                      strconv.FormatBool(incoming.Routing.SessionAffinity),
		"routing_session_affinity_ttl":                  incoming.Routing.SessionAffinityTTL,
		"routing_nvidia_poll_interval_ms":               strconv.Itoa(incoming.Routing.NvidiaPollIntervalMs),
		"routing_queue_max_depth":                       strconv.Itoa(incoming.Routing.QueueMaxDepth),
		"routing_queue_timeout_ms":                      strconv.Itoa(incoming.Routing.QueueTimeoutMs),
		"routing_health_failure_threshold":              strconv.Itoa(incoming.Routing.HealthFailureThreshold),
		"routing_health_success_threshold":              strconv.Itoa(incoming.Routing.HealthSuccessThreshold),
		"routing_overflow_sla_ms":                       strconv.Itoa(incoming.Routing.OverflowSLAMs),
		"routing_thermal_watchdog_enabled":              strconv.FormatBool(incoming.Routing.ThermalWatchdog.Enabled),
		"routing_thermal_watchdog_max_temp_celsius":     strconv.FormatFloat(incoming.Routing.ThermalWatchdog.MaxTempCelsius, 'f', -1, 64),
		"routing_thermal_watchdog_consecutive_breaches": strconv.Itoa(incoming.Routing.ThermalWatchdog.ConsecutiveBreaches),

		// Docker auto-discovery.
		"docker_enabled":          strconv.FormatBool(incoming.Docker.Enabled),
		"docker_socket":           incoming.Docker.Socket,
		"docker_poll_interval_ms": strconv.Itoa(incoming.Docker.PollIntervalMs),

		// Audit, webhooks, savings.
		"audit_enabled":                 strconv.FormatBool(incoming.Audit.Enabled),
		"audit_retention_days":          strconv.Itoa(incoming.Audit.RetentionDays),
		"audit_system_retention_days":   strconv.Itoa(incoming.Audit.SystemAuditRetentionDays),
		"webhook_enabled":               strconv.FormatBool(incoming.Webhook.Enabled),
		"webhook_url":                   incoming.Webhook.URL,
		"webhook_secret":                incoming.Webhook.Secret,
		"savings_reference_cost_per_1k": strconv.FormatFloat(incoming.Savings.ReferenceCostPer1K, 'f', -1, 64),

		// Global warmup (distinct from the per-node toggle in Warmup.tsx).
		"warmup_enabled":     strconv.FormatBool(incoming.Warmup.Enabled),
		"warmup_interval_ms": strconv.Itoa(incoming.Warmup.IntervalMs),
		"warmup_keep_alive":  incoming.Warmup.KeepAlive,
	}
	for key, val := range scalarSettings {
		if err := s.st.SetSetting(key, val); err != nil {
			log.Printf("admin: failed to persist %s setting: %v", key, err)
		}
	}

	// List/map-typed fields: JSON-encoded, not representable as a single
	// scalar settings value.
	jsonSettings := map[string]any{
		"warmup_models":           incoming.Warmup.Models,
		"routing_fallback_chains": incoming.Routing.FallbackChains,
		"context_windows":         incoming.ContextWindows,
	}
	for key, val := range jsonSettings {
		if err := store.SetJSONSetting(s.st, key, val); err != nil {
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
	// Trim here (not just in handleSummary) so tokenEvents can't grow
	// unbounded when nothing is polling the summary endpoint.
	s.tokenEvents = append(s.tokenEvents, TokenEvent{Time: now, Tokens: tokens})
	cutoff := now.Add(-time.Minute)
	kept := s.tokenEvents[:0]
	for _, e := range s.tokenEvents {
		if e.Time.After(cutoff) {
			kept = append(kept, e)
		}
	}
	s.tokenEvents = kept
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

	// Cloud nodes are stored as "cloud:<name>" (e.g. "cloud:openai") - see
	// the same check in handleLiveRequests.
	isCloud := strings.HasPrefix(node, "cloud:")
	s.auditLog.Log(audit.Entry{
		Time:      now,
		RequestID: id,
		KeyName:   apiKey,
		Model:     model,
		Node:      node,
		Status:    status,
		LatencyMs: latencyMs,
		Cloud:     isCloud,
	})
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
// was ever parsed - the UI renders that as "-" instead of a fabricated number.
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
		Priority        int     `json:"priority"`
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
			Priority:        cp.Priority,
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

	type nodeSnapshot struct {
		url     string
		name    string
		healthy bool
		warmSet map[string]bool
	}
	snapshots := make([]nodeSnapshot, len(nodes))

	for i, n := range nodes {
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
		snapshots[i] = nodeSnapshot{url: nodeURL, name: nodeName, healthy: nodeHealthy, warmSet: warmSet}
	}

	// Also include models that are installed on disk but not currently loaded.
	// FetchModelTags queries /api/tags which returns all available models. Each
	// call is a live HTTP round-trip (up to 5s) to a node - run them concurrently
	// so a slow/degraded node adds seconds, not minutes, on a 4-20 node fleet.
	type tagsResult struct {
		snap nodeSnapshot
		tags []router.TagModel
	}
	results := make([]tagsResult, len(snapshots))
	var wg sync.WaitGroup
	for i, snap := range snapshots {
		if !snap.healthy || snap.url == "" {
			continue
		}
		wg.Add(1)
		go func(i int, snap nodeSnapshot) {
			defer wg.Done()
			if tags, err := s.router.FetchModelTags(snap.url); err == nil {
				results[i] = tagsResult{snap: snap, tags: tags}
			}
		}(i, snap)
	}
	wg.Wait()

	for _, res := range results {
		for _, tm := range res.tags {
			if res.snap.warmSet[tm.Name] {
				continue // already added with warm count above
			}
			if modelMap[tm.Name] == nil {
				modelMap[tm.Name] = &modelEntry{Name: tm.Name}
			}
			modelMap[tm.Name].Nodes = append(modelMap[tm.Name].Nodes, nodeInfo{
				Name:    res.snap.name,
				Healthy: res.snap.healthy,
			})
			// WarmCount stays 0: model is available but not in VRAM
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
// finish (direct-to-Ollama streaming read, or the agent dispatch call)
// before giving up and marking the job failed. Model pulls, especially
// Hugging Face-sourced GGUF files (fetched directly from huggingface.co
// rather than Ollama's CDN-backed registry), routinely take much longer than
// a typical multi-GB-per-minute registry pull - a short timeout here would
// abort an otherwise-successful pull mid-download.
// A var (not const) so tests can override it to keep test runtimes short.
var nodePullTimeout = 2 * time.Hour

// pullJobMaxAge bounds how long a finished (success/failed) job stays in
// s.pullJobs after completion - long enough for a client's SSE subscription
// to catch the terminal event even if it connects a little late, short
// enough that the map doesn't grow unbounded across a long-running mesh
// process with many pulls over time.
const pullJobMaxAge = 10 * time.Minute

// pullJob tracks one in-flight or recently-finished model pull for the
// progress UI (GET .../pull/progress). Real numbers only, never fabricated
// (R1): BytesTotal/BytesCompleted are populated only for the direct-to-
// Ollama streaming path, which is the only path that actually reports byte
// counts today - Method distinguishes this so the UI knows to show a
// determinate progress bar (direct) vs an elapsed-time-only indicator
// (agent - see .local/specs/node-agent.md section 16, agent dispatch is a
// single blocking call with no incremental progress yet).
type pullJob struct {
	mu             sync.Mutex
	Node           string    `json:"node"`
	Model          string    `json:"model"`
	Method         string    `json:"method"` // "direct" | "agent"
	Status         string    `json:"status"` // "downloading" | "success" | "failed" | "cancelled"
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	BytesTotal     int64     `json:"bytes_total,omitempty"`
	BytesCompleted int64     `json:"bytes_completed,omitempty"`
	Error          string    `json:"error,omitempty"`
	// cancel aborts the in-flight pull's context - unexported, so it never
	// appears in the JSON progress payload the UI reads. Canceling the
	// mesh's own outbound HTTP request (direct path: the streaming pull;
	// agent path: the call to the agent) is real cancellation, not cosmetic:
	// Go's http.Server cancels the handler's request context when the
	// client connection drops, and the agent's action handler ties
	// exec.CommandContext to that same context - so a cancel from the admin
	// UI actually kills the download subprocess on the node, not just the
	// mesh's view of it.
	cancel context.CancelFunc
}

// pullJobSnapshot is pullJob's data with no mutex/cancel func - the type
// snapshot() returns and JSON-encodes, so a copy is always safe to pass
// around and vet never flags a struct-with-mutex copy.
type pullJobSnapshot struct {
	Node           string    `json:"node"`
	Model          string    `json:"model"`
	Method         string    `json:"method"`
	Status         string    `json:"status"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	BytesTotal     int64     `json:"bytes_total,omitempty"`
	BytesCompleted int64     `json:"bytes_completed,omitempty"`
	Error          string    `json:"error,omitempty"`
}

// snapshot returns a copy of j's data safe to JSON-encode without holding
// j.mu across the write.
func (j *pullJob) snapshot() pullJobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return pullJobSnapshot{
		Node: j.Node, Model: j.Model, Method: j.Method, Status: j.Status,
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		BytesTotal: j.BytesTotal, BytesCompleted: j.BytesCompleted, Error: j.Error,
	}
}

func (j *pullJob) setProgress(total, completed int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.BytesTotal, j.BytesCompleted = total, completed
}

// finish sets a terminal status, unless one is already set - a cancel
// requested from the admin UI races the pull goroutine's own eventual
// failure/success report (the cancelled context surfaces as a request
// error a moment later); the explicit "cancelled" outcome must win over
// whatever generic error the goroutine sees as a side effect of that same
// cancellation, not be silently overwritten by it.
func (j *pullJob) finish(status, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status != "downloading" {
		return
	}
	j.Status = status
	j.Error = errMsg
	j.FinishedAt = time.Now()
}

// requestCancel marks j cancelled (if still downloading) and invokes its
// context cancel func. Returns false if the job was already terminal - the
// caller uses this to tell "cancelled" from "too late, already finished".
func (j *pullJob) requestCancel() bool {
	j.mu.Lock()
	if j.Status != "downloading" {
		j.mu.Unlock()
		return false
	}
	j.Status = "cancelled"
	j.Error = "cancelled by admin"
	j.FinishedAt = time.Now()
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// sweepOldPullJobs removes finished jobs older than pullJobMaxAge. Called
// opportunistically from handleNodePull (bounded by how often pulls are
// started - no separate ticker/goroutine needed for what is, in practice, a
// tiny map).
func (s *Server) sweepOldPullJobs() {
	s.pullsMu.Lock()
	defer s.pullsMu.Unlock()
	for key, j := range s.pullJobs {
		snap := j.snapshot()
		if snap.Status != "downloading" && time.Since(snap.FinishedAt) > pullJobMaxAge {
			delete(s.pullJobs, key)
		}
	}
}

// handleNodePull starts an async model pull on a specific node.
// Accepts: {"model": "llama3:8b"}. Returns 202 immediately - the pull runs
// in the background; progress is polled via GET .../pull/progress (SSE).
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

	// A down node's URL may still be answering (e.g. some other service
	// listening on that port), producing a confusing upstream error that
	// looks model-specific when the real problem is just node reachability.
	// Fail fast with an honest reason instead.
	if !nodeIsHealthy(s.router.Nodes(), nodeName) {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("node %q is currently unreachable (down) - check its URL/connectivity before pulling", nodeName))
		return
	}

	s.sweepOldPullJobs()

	// Dedup concurrent pulls of the same model on the same node. State is
	// ephemeral and in-memory only - it is never persisted and never wired
	// into placement/warm-residency scoring, it just prevents two admin
	// clicks from racing the same multi-GB download.
	pullKey := nodeName + "|" + body.Model
	s.pullsMu.Lock()
	if existing, ok := s.pullJobs[pullKey]; ok && existing.snapshot().Status == "downloading" {
		s.pullsMu.Unlock()
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("pull already in progress for %q on node %q", body.Model, nodeName))
		return
	}

	agentCfg, agentOK := s.router.NodeAgentSetting(nodeName)
	useAgent := agentOK && agentCfg.Enabled && nodeHasAgentCapability(s.router.Nodes(), nodeName, "models.pull")
	method := "direct"
	if useAgent {
		method = "agent"
	}
	// Detached context: this request returns immediately (202), so the pull
	// itself must not be tied to r.Context(), which is canceled the moment
	// the handler returns. Built before the job is published to s.pullJobs
	// (not after, under a separate lock) so a DELETE .../pull landing the
	// instant this job becomes visible can never observe a nil job.cancel -
	// requestCancel() would silently no-op the real download in that window.
	pullCtx, cancel := context.WithTimeout(context.Background(), nodePullTimeout)
	job := &pullJob{Node: nodeName, Model: body.Model, Method: method, Status: "downloading", StartedAt: time.Now(), cancel: cancel}
	s.pullJobs[pullKey] = job
	s.pullsMu.Unlock()

	if useAgent {
		go func() {
			defer cancel()
			err := s.pullModelViaAgent(pullCtx, nodeURL, agentCfg, body.Model)
			if err != nil {
				job.finish("failed", err.Error())
				return
			}
			job.finish("success", "")
			s.logSystemChange(r, "pull_model", nodeName, fmt.Sprintf("Model: %s (via agent)", body.Model))
		}()
	} else {
		go func() {
			defer cancel()
			s.runDirectPull(pullCtx, job, nodeURL, body.Model)
			if job.snapshot().Status == "success" {
				s.logSystemChange(r, "pull_model", nodeName, fmt.Sprintf("Model: %s", body.Model))
			}
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"node":   nodeName,
		"model":  body.Model,
		"status": "downloading",
	})
}

// runDirectPull streams Ollama's /api/pull (stream:true) and updates job
// with real byte counts as they arrive - the only path that can report an
// honest total/completed (R1: never fabricate a progress percentage).
func (s *Server) runDirectPull(ctx context.Context, job *pullJob, nodeURL, model string) {
	pullBody, err := json.Marshal(map[string]interface{}{"model": model, "stream": true})
	if err != nil {
		job.finish("failed", err.Error())
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nodeURL+"/api/pull", bytes.NewReader(pullBody))
	if err != nil {
		job.finish("failed", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: nodePullTimeout}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("runDirectPull: request to node %s failed: %v", job.Node, err)
		job.finish("failed", fmt.Sprintf("pull failed for node %s: %v", job.Node, err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		upstreamMsg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		log.Printf("runDirectPull: node %s upstream returned %d: %s", job.Node, resp.StatusCode, upstreamMsg)
		errMsg := fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, strings.TrimSpace(string(upstreamMsg)))
		if resp.StatusCode == http.StatusUnauthorized {
			// Direct pulls hit the node's Ollama REST API directly, which has
			// no field for a Hugging Face token - only a Node Agent can
			// deliver one, via HF_TOKEN in the pull subprocess's own
			// environment (actions.go runDownload). A 401 here almost always
			// means a gated/token-required HF model on a node without an
			// agent, not a mesh misconfiguration.
			errMsg += " (this node has no Node Agent capable of pull_model - token-gated Hugging Face pulls require one; install/enable the Node Agent on this node or use a non-gated model)"
		}
		job.finish("failed", errMsg)
		return
	}

	// Ollama reports total/completed per layer (manifest, each blob, params,
	// license, ...), not as a single running aggregate - naively forwarding
	// each line's total/completed makes the bar jump back down every time a
	// new layer starts. Track every layer's own total/completed by digest
	// and sum them, so the reported progress only ever climbs across the
	// whole pull (still real, server-reported bytes - just correctly added
	// up, not fabricated - R1).
	dec := json.NewDecoder(resp.Body)
	var lastErr string
	layerTotal := make(map[string]int64)
	layerCompleted := make(map[string]int64)
	var cumulativeTotal, cumulativeCompleted int64
	for {
		var line struct {
			Status    string `json:"status"`
			Error     string `json:"error"`
			Digest    string `json:"digest"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
		}
		if err := dec.Decode(&line); err != nil {
			if err == io.EOF {
				break
			}
			job.finish("failed", fmt.Sprintf("reading pull progress: %v", err))
			return
		}
		if line.Error != "" {
			lastErr = line.Error
			continue
		}
		if line.Total > 0 && line.Digest != "" {
			cumulativeTotal += line.Total - layerTotal[line.Digest]
			layerTotal[line.Digest] = line.Total
			cumulativeCompleted += line.Completed - layerCompleted[line.Digest]
			layerCompleted[line.Digest] = line.Completed
			job.setProgress(cumulativeTotal, cumulativeCompleted)
		}
	}
	if lastErr != "" {
		job.finish("failed", lastErr)
		return
	}
	job.finish("success", "")
}

// handlePullProgress streams a pull job's state as Server-Sent Events until
// it reaches a terminal state (success/failed) or the client disconnects.
// GET .../pull/progress?model=...
func (s *Server) handlePullProgress(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("name")
	model := r.URL.Query().Get("model")
	pullKey := nodeName + "|" + model

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// This connection legitimately stays open for as long as the pull takes
	// (often many minutes for multi-GB models), far past adminHttpSrv's
	// server-wide 30s WriteTimeout (main.go). That timeout exists to bound
	// slow/stuck clients on ordinary request/response routes; it is not
	// meant for a deliberately long-lived SSE stream, so this handler alone
	// is exempted from it rather than raising the timeout server-wide.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	for {
		s.pullsMu.Lock()
		job, ok := s.pullJobs[pullKey]
		s.pullsMu.Unlock()
		if !ok {
			fmt.Fprintf(w, "event: not_found\ndata: {}\n\n")
			flusher.Flush()
			return
		}

		snap := job.snapshot()
		data, _ := json.Marshal(snap)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()

		if snap.Status != "downloading" {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// handleCancelPull aborts an in-flight pull job. DELETE .../pull?model=...
// Cancellation is real, not cosmetic: it cancels the mesh's own outbound
// request context (the streaming pull, or the call to the agent), which for
// the agent path also tears down the download subprocess on the node itself
// (see pullJob.cancel's doc comment). Returns 200 whether or not a job was
// actually still downloading to cancel - the admin UI's confirm step is
// what decides whether to call this at all; a late click racing the
// download's own natural completion isn't an error.
func (s *Server) handleCancelPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte(`{"error":"method not allowed"}`))
		return
	}

	nodeName := r.PathValue("name")
	model := r.URL.Query().Get("model")
	pullKey := nodeName + "|" + model

	s.pullsMu.Lock()
	job, ok := s.pullJobs[pullKey]
	s.pullsMu.Unlock()

	cancelled := false
	if ok {
		cancelled = job.requestCancel()
	}
	if cancelled {
		s.logSystemChange(r, "pull_model_cancel", nodeName, fmt.Sprintf("Model: %s", model))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "cancelled": cancelled})
}

// nodeIsHealthy reports whether the node named name is currently marked
// healthy by the router's poller. Returns false if the node isn't found -
// callers already validated existence via NodeURLs() before reaching here.
func nodeIsHealthy(nodes []*router.NodeState, name string) bool {
	for _, n := range nodes {
		if n.Name != name {
			continue
		}
		n.RLock()
		healthy := n.Healthy
		n.RUnlock()
		return healthy
	}
	return false
}

// nodeHasAgentCapability reports whether the node named name currently has
// an agent whose live-polled AgentCapabilities (agent_poll.go) includes
// capability.
func nodeHasAgentCapability(nodes []*router.NodeState, name, capability string) bool {
	for _, n := range nodes {
		if n.Name != name {
			continue
		}
		n.RLock()
		caps := n.AgentCapabilities
		n.RUnlock()
		for _, c := range caps {
			if c == capability {
				return true
			}
		}
		return false
	}
	return false
}

// pullModelViaAgent dispatches a model pull to nodeURL's Node Agent
// (POST /v1/models, capability "models.pull") instead of the node's own
// runtime HTTP API, forwarding the mesh's configured Hugging Face token
// per-request - never stored on the agent side, only set in the pull
// subprocess's own environment for its lifetime (see
// .local/specs/node-agent.md section 16).
func (s *Server) pullModelViaAgent(ctx context.Context, nodeURL string, agentCfg router.NodeAgentConfig, model string) error {
	actionURL, err := buildAgentActionURL(nodeURL, agentCfg.Port)
	if err != nil {
		return err
	}

	reqBody, err := json.Marshal(map[string]string{"model": model, "hf_token": s.cfg.HuggingFace.Token})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, nodePullTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentCfg.Token)

	client := &http.Client{Timeout: nodePullTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("agent pull failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("agent pull: could not decode response (status %d)", resp.StatusCode)
	}
	if !out.OK {
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("agent returned %d", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

// buildAgentActionURL derives the agent's POST /v1/models URL from the
// node's own URL (same host) and the configured agent port, via url.Parse
// per R5 - never arithmetic port derivation. Mirrors agent_poll.go's
// buildAgentURL in internal/router (kept as a separate small function since
// admin and router are different packages).
func buildAgentActionURL(nodeURL string, port int) (string, error) {
	u, err := url.Parse(nodeURL)
	if err != nil {
		return "", fmt.Errorf("parse node URL: %w", err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("node URL %q has no host", nodeURL)
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d/v1/models", scheme, u.Hostname(), port), nil
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
		writeServerError(w, r, err)
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
		writeServerError(w, r, err)
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

// vramAgentVendorToolLabel maps a Node Agent's detected GPU vendor
// (nodeagent.GPUBlock.Vendor - "nvidia"/"rocm"/"intel"/"apple") to the actual
// command-line tool it read from, mirroring ui/src/components/VramBar.tsx's
// AGENT_VENDOR_LABEL so the admin API and the GPU Nodes card never disagree
// about what an agent-sourced reading's real source was.
var vramAgentVendorToolLabel = map[string]string{
	"nvidia": "nvidia-smi",
	"rocm":   "rocm-smi",
	"intel":  "xpu-smi",
	"apple":  "system_profiler",
}

// vramFitSourceLabel names the tool that actually produced a node's VRAM
// total, given its raw VRAMSource ("nvidia"/"agent"/"declared"/"api"/"none")
// and (when the source is "agent") the vendor the Node Agent detected. Only
// called when vramTotalMB > 0, so the caller has already established there
// is a real total to attribute - this just names its source honestly instead
// of defaulting every non-"nvidia"/"declared" case to a claimed "nvidia-smi"
// origin regardless of which vendor tool actually produced it (R1: a label
// is a claim about provenance, not a decoration).
func vramFitSourceLabel(rawVramSource, agentGPUVendor string) string {
	switch rawVramSource {
	case "nvidia":
		return "nvidia-smi"
	case "declared":
		return "declared"
	case "agent":
		if label, ok := vramAgentVendorToolLabel[agentGPUVendor]; ok {
			return label
		}
		return "agent"
	default:
		return "agent"
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
		agentGPUVendor := n.AgentGPUVendor
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
			vramSource = vramFitSourceLabel(rawVramSource, agentGPUVendor)
		} else if vramUsedMBFromPS > 0 {
			// No nvidia-smi but we have ps data - use loaded model VRAM as lower bound.
			vramTotalBytes = 0
			vramFreeBytes = 0
			vramSource = "inferred"
		} else {
			vramSource = "unknown"
		}

		// Fetch downloaded models from /api/tags (cached 30s in router).
		tagModels, err := s.router.FetchModelTags(nodeURL)
		if err != nil {
			// Node unreachable - emit an empty entry so the UI still shows the node.
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
