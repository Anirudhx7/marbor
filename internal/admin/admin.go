package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
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
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Anirudhx7/marbor/internal/audit"
	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/bench"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

//go:embed web/dist
var webFS embed.FS

type ctxKey string

const ctxKeyUsername ctxKey = "username"
const ctxKeyUserID ctxKey = "user_id"

// sessionCookieName is the httpOnly cookie holding the admin session token.
// The token itself never reaches client-side JS or localStorage (Priority 2,
// 2026-07-14 audit) - only this cookie carries it, and only the server reads
// it back.
const sessionCookieName = "marbor_session"

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
	statsChan      chan statsJob             // hourly bucket + model stat upserts, same async-drain pattern as logChan
	pullsMu        sync.Mutex                // guards pullJobs
	pullJobs       map[string]*pullJob       // "node|model" -> job state; ephemeral, never persisted
	coldStarts     int64                     // atomic - total cold start events
	warmHits       int64                     // atomic - total warm hit events
	tokenEvents    []TokenEvent              // protected by mu
	mgmtEndpoints  managementEndpointsSetter // nil until wired via SetProxyHandler
	benchMu        sync.Mutex                // guards benchJobs
	benchJobs      map[string]*benchmarkJob  // job id -> job state; ephemeral, never persisted (results land in benchmark_runs)
	backupMu       sync.Mutex                // guards lastBackupAt/lastBackupErr
	lastBackupAt   time.Time                 // zero = never (or not yet loaded from store)
	lastBackupErr  string                    // empty = last attempt (if any) succeeded
	restoreCh      chan<- string             // nil until SetRestoreChannel is called (main.go only, in a real run)
	enrollMu       sync.Mutex                // guards enrollCodes
	enrollCodes    map[string]enrollmentCode // one-time code -> record; ephemeral, never persisted (P50)
	// nodePatchMu serializes handlePatchNode's entire validate-then-mutate
	// transaction (validateTLSPatch through the final PatchNode/
	// UpdateNodeURL call). Without it, two concurrent PATCH requests to
	// DIFFERENT node names can each read a pre-mutation node-list snapshot,
	// both pass validateTLSPatch's sibling-consistency check against that
	// stale snapshot, and then both mutate - jointly producing two nodes on
	// one Host with different pinned TLS fingerprints, the exact state P24
	// section 15 exists to prevent. This does not touch mu (r.router's own
	// node-list lock already protects individual reads/writes; this mutex
	// only prevents two full PATCH transactions from interleaving).
	nodePatchMu sync.Mutex
	// settingsMu serializes handleUpdateSettings' entire read-validate-write
	// sequence. s.mu itself is only held briefly (snapshot, then final swap)
	// so a slow/large request body can't stall cors()/LogRequest on every
	// other request, but that means two concurrent settings updates could
	// otherwise both validate against the same stale snapshot and the second
	// write would silently discard the first's changes (lost update) -
	// same nodePatchMu discipline as handlePatchNode above.
	settingsMu sync.Mutex
	// scheduleMu serializes handleCreateSchedule/handlePatchSchedule/
	// handleDeleteSchedule's entire load-validate-store sequence - each
	// reads the full schedule list, mutates a local copy, and replaces the
	// whole list via persistSchedules, with no serialization between the
	// three; two concurrent calls can otherwise each snapshot the same
	// pre-mutation list and one's write silently discards the other's
	// (lost update), same class of bug nodePatchMu/settingsMu guard above.
	scheduleMu sync.Mutex
}

// enrollmentCode is a short-lived, single-use credential exchanged by a Node
// Agent (via POST /admin/agent/enroll) for its real, permanent bearer token,
// so the permanent token never has to travel through a copy-pasted install
// command, shell history, or CLI argv (P50). node is carried for audit
// logging only - the code itself is what the map is keyed by.
type enrollmentCode struct {
	node      string
	token     string
	expiresAt time.Time
}

// enrollmentCodeTTL bounds how long an unused enrollment code stays valid.
// Short enough to limit the exposure window of a code appearing in a pasted
// command, long enough for an operator to actually run the install.
const enrollmentCodeTTL = 20 * time.Minute

// SetRestoreChannel wires the channel handleRestoreBackup sends a validated
// backup file's full path down after a one-click restore request. main.go
// owns process-lifecycle decisions (graceful shutdown, file swap, exit code
// for the supervisor to restart on), so the HTTP handler here only validates
// the request and hands off - it never performs the swap or exits itself.
// Left nil in tests/demo/other run modes that never wire this: the handler
// returns 501 rather than silently accepting a restore it can't act on.
func (s *Server) SetRestoreChannel(ch chan<- string) { s.restoreCh = ch }

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
				ID:         rec.ID,
				ApiKey:     rec.KeyName,
				Model:      rec.Model,
				Node:       rec.NodeName,
				Status:     strconv.Itoa(rec.StatusCode),
				HTTPStatus: rec.StatusCode,
				Latency:    int(rec.LatencyMs),
				Tokens:     rec.TokensUsed,
				Time:       rec.TS,
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

	// Restore the last scheduled-backup outcome so a restart doesn't lose
	// track of it (and doesn't immediately re-run a backup that already
	// happened before the restart - see StartBackupScheduler).
	s.backupMu.Lock()
	if v, err := s.st.GetSetting("backup_last_at"); err == nil && v != "" {
		if t, perr := time.Parse(time.RFC3339, v); perr == nil {
			s.lastBackupAt = t
		}
	}
	if v, err := s.st.GetSetting("backup_last_error"); err == nil {
		s.lastBackupErr = v
	}
	s.backupMu.Unlock()

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
	HTTPStatus   int       `json:"httpStatus"`
	Latency      int       `json:"latency"`
	Tokens       int64     `json:"tokens"`
	TokensPerSec float64   `json:"tokensPerSec"`
	Time         time.Time `json:"time"`
	// RoutingReason is the P41 top-level explanation (session_affinity |
	// pinned_warm | score_based); empty for cloud-fallback requests, which
	// have no router.RoutingDecision. RoutingDetail is the JSON-encoded
	// router.RoutingDecision, kept off the /admin/requests list response
	// (only RoutingReason is serialized there) and used internally by the
	// /admin/v1/requests/{id}/explain handler for recent (in-memory-ring)
	// requests without a round-trip to SQLite.
	RoutingReason string `json:"routingReason,omitempty"`
	RoutingDetail string `json:"-"`
}

type nodeResp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
	// Scheme is this node's URL scheme ("http" or "https") - the UI needs
	// this to know whether TLS pinning is even applicable, and to correctly
	// pre-populate a scheme toggle when editing (P24; not present before
	// this - Host/Port alone don't carry it).
	Scheme   string `json:"scheme"`
	GPUModel string `json:"gpuModel"`
	// GPUIndices is the operator-declared set of physical GPU indices this
	// node/runtime instance actually uses (P75 Gap B/C) - empty/omitted means
	// nothing declared, unchanged host-level sizing. See NodeState.DeclaredGPUIndices.
	GPUIndices []int `json:"gpuIndices,omitempty"`
	// MaxInFlight is this node's declared per-node in-flight cap override
	// (P64) - 0 means no override is declared (the global
	// routing.max_in_flight_per_node default applies instead).
	MaxInFlight int `json:"maxInFlight,omitempty"`
	// TLSFingerprint is this node's TOFU-pinned Marbor Agent cert fingerprint
	// (P24) - empty/omitted means no pin (plaintext or not yet TLS-enrolled).
	// See .local/specs/node-agent-tls.md.
	TLSFingerprint string `json:"tlsFingerprint,omitempty"`
	// TLSFingerprintMismatch is true when the most recent agent poll failed
	// specifically because the presented certificate didn't match the
	// pinned fingerprint (P24 section 6) - distinct from generic
	// unreachability, so the UI can show its own status instead of
	// "unreachable" (which would send an operator debugging network
	// connectivity when the real cause is a stale pin).
	TLSFingerprintMismatch bool `json:"tlsFingerprintMismatch,omitempty"`
	// ParallelismType/Width is the deployment topology (P397) - type tp|pp|ep|dp,
	// width 1..64, derived EffectiveRequiredGPUs = max(len(gpuIndices), width)
	// - 0/empty means unconstrained (existing fleet).
	ParallelismType       string `json:"parallelismType,omitempty"`
	ParallelismWidth      int    `json:"parallelismWidth,omitempty"`
	EffectiveRequiredGPUs int    `json:"effectiveRequiredGPUs,omitempty"`
	// P397b: auto-discovered deployment (additive, per-port). Declared above
	// always overrides detected - effectiveRequiredGPUs already prefers
	// declared when present. Detected fields are honest "what agent saw"
	// (R1) via ps/docker/env, not fabricated. MismatchWarning is amber
	// warning when declared 4 vs detected 8 (not a 422 block - Adopt fixes).
	DetectedParallelismType       string             `json:"detectedParallelismType,omitempty"`
	DetectedParallelismWidth      int                `json:"detectedParallelismWidth,omitempty"`
	DetectedGPUGroup              []int              `json:"detectedGPUGroup,omitempty"`
	DetectedSource                string             `json:"detectedSource,omitempty"`
	DetectedRuntime               string             `json:"detectedRuntime,omitempty"`
	DetectedEffectiveRequiredGPUs int                `json:"detectedEffectiveRequiredGPUs,omitempty"`
	MismatchWarning               string             `json:"mismatchWarning,omitempty"`
	VRAMTotalMB                   int64              `json:"vramTotalMB"`
	VRAMUsedMB                    int64              `json:"vramUsedMB"`
	VRAMSource                    string             `json:"vramSource"`
	PowerDrawW                    float64            `json:"powerDrawW"`
	Temperature                   *float64           `json:"temperature"`
	Runtime                       string             `json:"runtime"`
	Health                        string             `json:"health"`
	Draining                      bool               `json:"draining"`
	DrainedReason                 string             `json:"drainedReason,omitempty"`
	PrewarmDisabled               bool               `json:"prewarmDisabled"`
	Uptime                        string             `json:"uptime"`
	LoadedModels                  []router.ModelInfo `json:"loadedModels"`
	// WarmupErrors is the last warmup-ping failure per model (model -> error
	// string) - populated only for models that failed to warm; a model that
	// warmed successfully or was never attempted has no entry. Lets the UI
	// show *why* a keep-warm model is stuck instead of leaving it silently
	// "not resident" forever (see NodeState.WarmupErrors in router.go).
	WarmupErrors map[string]string `json:"warmupErrors,omitempty"`
	// UnloadErrors mirrors WarmupErrors for the scheduled-unload path - the
	// last failed scheduled/agent unload per model (see NodeState.UnloadErrors
	// in router.go), so a schedule that reports "ok" (dispatch succeeded) but
	// whose actual unload failed is still diagnosable from the dashboard.
	UnloadErrors map[string]string `json:"unloadErrors,omitempty"`
	// WarmupState lists models currently suppressed on this node (a manual or
	// scheduled unload took them cold and they won't be reloaded until an
	// explicit warmup re-arms them, per router.suppressWarmup) - the
	// operator-facing shape of router.SuppressedWarmupInfo, never the raw
	// suppression map/bool. A model that's warm, cold-but-never-suppressed, or
	// failed (see WarmupErrors above) has no entry here.
	WarmupState      []warmupStateEntry `json:"warmupState,omitempty"`
	ActiveConns      int32              `json:"activeConns"`
	RequestsTotal    int64              `json:"requestsTotal"`
	HealthHistory    []float64          `json:"healthHistory"`
	PendingPrewarmMB int64              `json:"pendingPrewarmMB"`
	ColdStarts       int64              `json:"coldStarts"`
	TokensTotal      int64              `json:"tokensTotal"`
	AvgLatencyMs     float64            `json:"avgLatencyMs"`
	WarmHitRatio     float64            `json:"warmHitRatio"`
	// Marbor Agent-derived fields (internal/marboragent). AgentPresent is false
	// (and every other field below zero-value) whenever no agent is
	// configured for this node, or the most recent agent poll failed - the
	// UI must check AgentPresent before displaying FanPercent/RAMUsedMB/
	// DiskFreeGB/AgentVersion, never treat a zero as a real measurement (R1).
	AgentPresent bool `json:"agentPresent"`
	// AgentStale is true when an enabled Marbor Agent IS configured for this
	// node's host but its consecutive poll failures crossed the health
	// failure threshold (telemetry cleared) - i.e. the enrolled agent went
	// dark. Deliberately false both when the agent is healthy AND when no
	// agent was ever configured for this host, so a UI can alert on the
	// former without nagging fleets that run some nodes agentless by choice.
	// See router.NodeState.AgentStale for where the distinction is made.
	AgentStale   bool     `json:"agentStale,omitempty"`
	AgentVersion string   `json:"agentVersion,omitempty"`
	FanPercent   *float64 `json:"fanPercent"`
	CPUPercent   float64  `json:"cpuPercent"`
	RAMUsedMB    int64    `json:"ramUsedMB"`
	DiskFreeGB   float64  `json:"diskFreeGB"`
	// AgentCapabilities/AgentPlatform/AgentArchitecture/AgentGPUVendor/
	// AgentRuntime are the agent's self-reported metadata (see
	// internal/marboragent Telemetry.Capabilities/Platform/Architecture/
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
	// surviving agent upgrades/hostname changes - internal/marboragent
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

// warmupStateEntry is the operator-facing shape of one suppressed keep-warm
// model - see WarmupState on nodeResp above. Reason is one of
// "manual_unload"/"scheduled_unload" (router.suppressedInfo.Reason); Since is
// RFC3339 UTC, the moment the suppression was set.
type warmupStateEntry struct {
	Model  string `json:"model"`
	State  string `json:"state"` // always "suppressed" today - see WarmupState doc comment
	Reason string `json:"reason"`
	Since  string `json:"since"`
}

// agentGPUDevice is the admin API's camelCase projection of
// marboragent.GPUInfo (whose own JSON tags are snake_case, matching the
// Marbor Agent Protocol wire format, not this admin API's convention) - one
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
func toAgentGPUDevices(devices []marboragent.GPUInfo) []agentGPUDevice {
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
	CPUCores   int    `json:"cpu_cores"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	RAMTotalMB int64  `json:"ram_total_mb"`
	RAMFreeMB  int64  `json:"ram_free_mb"`
	// RAMKnown is false when readSystemMemory couldn't actually read the
	// host's memory (as opposed to RAMTotalMB/RAMFreeMB being 0 because
	// they're genuinely unset) - R1: real data or unknown, never a fake 0
	// presented as a measurement.
	RAMKnown   bool          `json:"ram_known"`
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
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	Key                   string   `json:"key"`
	Created               string   `json:"created"`
	RequestsToday         int      `json:"requestsToday"`
	RequestsThisMonth     int      `json:"requestsThisMonth"`
	TokensThisMonth       int64    `json:"tokensThisMonth"`
	EstimatedCostUsd      float64  `json:"estimatedCostUsd"`
	RateLimit             int      `json:"rateLimit"`
	DailyUsdCap           float64  `json:"dailyUsdCap,omitempty"`
	MonthlyUsdCap         float64  `json:"monthlyUsdCap,omitempty"`
	Status                string   `json:"status"`
	AllowedModels         []string `json:"allowedModels"`
	ExpiresAt             string   `json:"expiresAt,omitempty"`
	LocalOnly             bool     `json:"localOnly,omitempty"`
	AllowLocalDegradation bool     `json:"allowLocalDegradation,omitempty"`
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
		statsChan:      make(chan statsJob, 5000),
		pullJobs:       make(map[string]*pullJob),
		benchJobs:      make(map[string]*benchmarkJob),
		enrollCodes:    make(map[string]enrollmentCode),
	}
	s.ensureAdminUser()
	s.logWg.Add(1)
	go s.startAsyncLogger()
	return s
}

// statsJob bundles the hourly-bucket and model-stat writes TrackLocalRequestModel
// makes per local request, so both go through one async queue entry instead
// of two separate blocking SQLite calls. TrackCloudCostModel does NOT use
// this - its write stays synchronous, see the comment on that function.
type statsJob struct {
	bucket store.HourlyBucket
	stat   store.ModelStat
}

func (s *Server) startAsyncLogger() {
	defer s.logWg.Done()
	for {
		select {
		case rec := <-s.logChan:
			_ = s.st.AppendRequest(rec)
		case job := <-s.statsChan:
			_ = s.st.UpsertHourlyBucket(job.bucket)
			_ = s.st.UpsertModelStat(job.stat)
		case <-s.logDone:
			// Drain whatever is already buffered, then stop. logChan and
			// statsChan are never closed (LogRequest/TrackLocalRequestModel
			// keep sending on them via a non-blocking select even after
			// Shutdown), so this only races benignly: anything enqueued after
			// the drain below just sits unread, it never panics on a closed
			// channel.
			for {
				select {
				case rec := <-s.logChan:
					_ = s.st.AppendRequest(rec)
				case job := <-s.statsChan:
					_ = s.st.UpsertHourlyBucket(job.bucket)
					_ = s.st.UpsertModelStat(job.stat)
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
// hours (plus once at startup, so a long-idle marbor doesn't wait 12h for its
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

// backupCheckInterval is how often StartBackupScheduler wakes up to check
// whether a scheduled backup is due. It is intentionally much shorter than
// the configurable IntervalHours (which can be changed live via Settings
// without a restart) - each wake checks elapsed time since the last
// successful backup against the *current* cfg.Backup.IntervalHours, rather
// than fixing the ticker period at whatever interval was configured at boot.
const backupCheckInterval = 15 * time.Minute

// StartBackupScheduler launches a background goroutine that runs a scheduled
// marbor.db backup (VACUUM INTO cfg.Backup.TargetDir) whenever it is enabled
// and due, then prunes old backup files beyond cfg.Backup.RetentionCount.
// Call once after construction; ctx cancellation stops the ticker.
func (s *Server) StartBackupScheduler(ctx context.Context) {
	check := func() {
		s.mu.RLock()
		cfg := s.cfg.Backup
		s.mu.RUnlock()
		if !cfg.Enabled {
			return
		}
		intervalHours := cfg.IntervalHours
		if intervalHours <= 0 {
			intervalHours = 24
		}
		s.backupMu.Lock()
		due := time.Since(s.lastBackupAt) >= time.Duration(intervalHours)*time.Hour
		s.backupMu.Unlock()
		if !due {
			return
		}
		if err := s.runScheduledBackup(cfg); err != nil {
			log.Printf("admin: scheduled backup failed: %v", err)
		}
	}
	go func() {
		check() // in case a backup came due while marbor was stopped
		ticker := time.NewTicker(backupCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}

// runScheduledBackup performs one scheduled backup run: VACUUM INTO a
// timestamped file under cfg.TargetDir, record the outcome (surfaced by
// handleSettings), then prune old backups beyond cfg.RetentionCount.
func (s *Server) runScheduledBackup(cfg config.BackupConfig) error {
	if cfg.TargetDir == "" {
		err := fmt.Errorf("no backup target directory configured")
		s.recordBackupResult(err)
		return err
	}
	if err := os.MkdirAll(cfg.TargetDir, 0o755); err != nil {
		err = fmt.Errorf("create backup target directory %s: %w", cfg.TargetDir, err)
		s.recordBackupResult(err)
		return err
	}
	// Staged under a .tmp suffix first (doesn't match backupFilenameRE, so
	// pruneOldBackups/findDuplicateBackup/handleListBackups all ignore it)
	// so a backup identical to one already on disk - e.g. marbor.db hasn't
	// changed since the last scheduled run - can be discarded instead of
	// cluttering the restore list with a byte-for-byte duplicate.
	tmpPath := filepath.Join(cfg.TargetDir, backupFilename(time.Now())+".tmp")
	if err := s.st.BackupTo(tmpPath); err != nil {
		s.recordBackupResult(err)
		return err
	}
	hash, err := hashBackupFile(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		s.recordBackupResult(err)
		return err
	}
	if dup, err := findDuplicateBackup(cfg.TargetDir, hash); err == nil && dup != "" {
		os.Remove(tmpPath)
		s.recordBackupResult(nil) // still a successful run - don't retry every tick
		return nil
	}
	finalPath := strings.TrimSuffix(tmpPath, ".tmp")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		s.recordBackupResult(err)
		return err
	}
	s.recordBackupResult(nil)
	s.pruneOldBackups(cfg.TargetDir, cfg.RetentionCount)
	return nil
}

// backupFilename returns the marbor-backup-<UTC timestamp>.db name used by both
// the manual download endpoint and the scheduled job, so pruneOldBackups can
// recognize either kind of file left in TargetDir.
func backupFilename(t time.Time) string {
	return fmt.Sprintf("marbor-backup-%s.db", t.UTC().Format("20060102-150405"))
}

// recordBackupResult updates the in-memory last-backup status and persists it
// so it survives a restart. A successful run (err == nil) advances
// lastBackupAt and clears the error; a failed run leaves lastBackupAt
// unchanged (so the next scheduler tick retries) but records the error for
// the Settings page (R1: real state, never a fabricated "backed up" status).
func (s *Server) recordBackupResult(err error) {
	s.backupMu.Lock()
	if err == nil {
		s.lastBackupAt = time.Now()
		s.lastBackupErr = ""
	} else {
		s.lastBackupErr = err.Error()
	}
	at, errStr := s.lastBackupAt, s.lastBackupErr
	s.backupMu.Unlock()

	if !at.IsZero() {
		if perr := s.st.SetSetting("backup_last_at", at.UTC().Format(time.RFC3339)); perr != nil {
			log.Printf("admin: failed to persist backup_last_at setting: %v", perr)
		}
	}
	if perr := s.st.SetSetting("backup_last_error", errStr); perr != nil {
		log.Printf("admin: failed to persist backup_last_error setting: %v", perr)
	}
}

// pruneOldBackups deletes the oldest marbor-backup-*.db files in dir beyond
// retentionCount. Filenames are UTC-timestamp-named (backupFilename), so a
// lexical sort is also a chronological sort. retentionCount <= 0 disables
// pruning entirely (keep every backup ever made).
func (s *Server) pruneOldBackups(dir string, retentionCount int) {
	if retentionCount <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("admin: could not list backup directory %s for pruning: %v", dir, err)
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "marbor-backup-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) <= retentionCount {
		return
	}
	for _, name := range names[:len(names)-retentionCount] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Printf("admin: failed to prune old backup %s: %v", name, err)
		}
	}
}

// backupDedupIgnoredSettingsKeys are settings rows excluded from the dedup
// hash (hashBackupFile) because runScheduledBackup writes them into the live
// store itself right after every successful run, scheduled or skipped. Left
// in, that write would poison every future backup's snapshot with the
// previous run's own completion timestamp, so two backups of an otherwise
// byte-for-byte-unchanged marbor.db would never hash-equal past the first.
var backupDedupIgnoredSettingsKeys = []string{"backup_last_at", "backup_last_error"}

// hashBackupFile returns the SHA-256 hex digest of the file at path, used to
// detect a backup that is byte-for-byte identical to one already on disk.
// It hashes a scratch copy with backupDedupIgnoredSettingsKeys stripped out
// first, never the file at path itself - path may be an existing kept backup
// (or, for the upload endpoint, an operator's file) that must not be mutated.
func hashBackupFile(path string) (string, error) {
	scratch, err := os.CreateTemp(filepath.Dir(path), "backup-dedup-*.db")
	if err != nil {
		return "", err
	}
	scratchPath := scratch.Name()
	defer os.Remove(scratchPath)

	src, err := os.Open(path)
	if err != nil {
		scratch.Close()
		return "", err
	}
	_, copyErr := io.Copy(scratch, src)
	src.Close()
	closeErr := scratch.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}

	db, err := sql.Open("sqlite", scratchPath)
	if err != nil {
		return "", err
	}
	for _, key := range backupDedupIgnoredSettingsKeys {
		if _, err := db.Exec(`DELETE FROM settings WHERE key = ?`, key); err != nil {
			db.Close()
			return "", err
		}
	}
	// VACUUM recompacts into a canonical page layout - without it, two files
	// with identical post-delete logical content can still differ byte-for-
	// byte because their b-tree layout carries the imprint of a different
	// insert/delete history (e.g. one file's row was deleted just now, the
	// other's key was simply never present).
	if _, err := db.Exec(`VACUUM`); err != nil {
		db.Close()
		return "", err
	}
	if err := db.Close(); err != nil {
		return "", err
	}

	// Zero the SQLite header's file change counter (offsets 24-27) and its
	// mirror, the version-valid-for number (offsets 92-95, SQLite file
	// format spec). Both are incremented on every committed write
	// transaction, including a DELETE that touches zero rows - so a scratch
	// copy where backupDedupIgnoredSettingsKeys' DELETEs were no-ops (the
	// key was never present) ends up with a different counter value than a
	// copy where they actually removed rows, even once VACUUM has made
	// every other byte identical. Left unzeroed, that counter drift alone
	// causes a false hash mismatch on otherwise byte-for-byte-identical
	// content.
	scratchBytes, err := os.ReadFile(scratchPath)
	if err != nil {
		return "", err
	}
	for _, off := range [2]int{24, 92} {
		if off+4 <= len(scratchBytes) {
			for i := 0; i < 4; i++ {
				scratchBytes[off+i] = 0
			}
		}
	}

	h := sha256.New()
	h.Write(scratchBytes)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// findDuplicateBackup hashes every existing marbor-backup-*.db file in dir and
// returns the name of one whose content matches hash, or "" if none match.
// Used by both the scheduled backup job and the upload endpoint so a
// byte-for-byte duplicate (marbor.db unchanged since the last backup, or an
// operator re-uploading a file they already downloaded) never gets added to
// the restorable pool a second time.
func findDuplicateBackup(dir, hash string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() || !backupFilenameRE.MatchString(e.Name()) {
			continue
		}
		existingHash, err := hashBackupFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // unreadable/corrupt existing file - not this upload's problem
		}
		if existingHash == hash {
			return e.Name(), nil
		}
	}
	return "", nil
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

	// Go 1.22+ ServeMux returns a bare 405 (no handler invoked) for a method
	// that doesn't match a registered pattern when another method on the same
	// path does - so a cross-origin OPTIONS preflight for any method-specific
	// route (every PATCH/PUT/DELETE/POST route below) never reaches s.cors,
	// and the browser blocks the real request for lack of CORS headers. This
	// catch-all answers preflight for the whole /admin/ subtree via s.cors
	// itself, before any method-specific pattern is consulted.
	mux.HandleFunc("OPTIONS /admin/", s.cors(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

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
	reg("GET /admin/spill", s.cors(s.adminAuth(s.handleSpillCounters)))

	reg("GET /admin/routing/rules", s.cors(s.adminAuth(s.handleRoutingRules)))
	reg("POST /admin/routing/rules", s.cors(s.adminAuth(s.handleAddRoutingRule)))
	reg("DELETE /admin/routing/rules/{id}", s.cors(s.adminAuth(s.handleRemoveRoutingRule)))
	reg("PUT /admin/routing/rules/{id}/toggle", s.cors(s.adminAuth(s.handleToggleRoutingRule)))
	reg("GET /admin/routing/strategy", s.cors(s.adminAuth(s.handleGetRoutingStrategy)))
	reg("PUT /admin/routing/strategy", s.cors(s.adminAuth(s.handleSetRoutingStrategy)))

	reg("GET /admin/settings", s.cors(s.adminAuth(s.handleSettings)))
	reg("PUT /admin/settings", s.cors(s.adminAuth(s.handleUpdateSettings)))
	reg("POST /admin/backup", s.cors(s.adminAuth(s.handleBackupNow)))
	reg("GET /admin/backup/list", s.cors(s.adminAuth(s.handleListBackups)))
	reg("POST /admin/backup/restore", s.cors(s.adminAuth(s.handleRestoreBackup)))
	reg("POST /admin/backup/upload", s.cors(s.adminAuth(s.handleUploadBackup)))

	reg("GET /admin/requests", s.cors(s.adminAuth(s.handleRequests)))
	reg("GET /admin/requests/live", s.cors(s.adminAuth(s.handleLiveRequests)))
	reg("GET /admin/requests/{id}/explain", s.cors(s.adminAuth(s.handleExplainRequest)))
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
	reg("GET /admin/nodes/{name}/models", s.cors(s.adminAuth(s.handleNodeModels)))
	// "{model...}" (not "{model}") deliberately - model names routinely
	// contain "/" (e.g. "org/repo"), same reasoning as the agent's own
	// DELETE /v1/models/{name...} route (server.go).
	reg("DELETE /admin/nodes/{name}/models/{model...}", s.cors(s.adminAuth(s.handleNodeDeleteModel)))
	reg("GET /admin/nodes/{name}/health-check", s.cors(s.adminAuth(s.handleNodeHealthCheck)))
	// P24: TLS enrollment probe - fetches the node's presented cert
	// fingerprint for operator confirmation, never pins it. See
	// .local/specs/node-agent-tls.md section 2.
	reg("POST /admin/nodes/{name}/tls-probe", s.cors(s.adminAuth(s.handleNodeTLSProbe)))
	reg("GET /admin/nodes/{name}/pull/progress", s.cors(s.adminAuth(s.handlePullProgress)))
	reg("GET /admin/pulls", s.cors(s.adminAuth(s.handleListActivePulls)))
	reg("DELETE /admin/nodes/{name}/pull", s.cors(s.adminAuth(s.handleCancelPull)))
	reg("POST /admin/nodes/{name}/drain", s.cors(s.adminAuth(s.handleDrainNode)))
	reg("DELETE /admin/nodes/{name}/drain", s.cors(s.adminAuth(s.handleUndrainNode)))
	reg("POST /admin/nodes/{name}/prewarm", s.cors(s.adminAuth(s.handleSetNodePrewarm)))
	reg("POST /admin/benchmark/run", s.cors(s.adminAuth(s.handleRunBenchmark)))
	reg("GET /admin/benchmark/{id}/progress", s.cors(s.adminAuth(s.handleBenchmarkProgress)))
	reg("DELETE /admin/benchmark/{id}", s.cors(s.adminAuth(s.handleCancelBenchmark)))
	reg("GET /admin/benchmark/runs", s.cors(s.adminAuth(s.handleListBenchmarkRuns)))
	reg("GET /admin/nodes/{name}/agent", s.cors(s.adminAuth(s.handleGetMarborAgent)))
	reg("POST /admin/nodes/{name}/agent", s.cors(s.adminAuth(s.handleEnableMarborAgent)))
	reg("DELETE /admin/nodes/{name}/agent", s.cors(s.adminAuth(s.handleDisableMarborAgent)))
	reg("POST /admin/nodes/{name}/agent/regenerate", s.cors(s.adminAuth(s.handleRegenerateMarborAgentToken)))
	reg("GET /admin/nodes/{name}/control", s.cors(s.adminAuth(s.handleGetNodeControl)))
	reg("POST /admin/nodes/{name}/control/accept", s.cors(s.adminAuth(s.handleAcceptNodeControl)))
	reg("DELETE /admin/nodes/{name}/control", s.cors(s.adminAuth(s.handleClearNodeControl)))
	reg("POST /admin/nodes/{name}/runtime/start", s.cors(s.adminAuth(s.handleNodeRuntimeStart)))
	reg("POST /admin/nodes/{name}/runtime/stop", s.cors(s.adminAuth(s.handleNodeRuntimeStop)))
	reg("POST /admin/nodes/{name}/runtime/restart", s.cors(s.adminAuth(s.handleNodeRuntimeRestart)))
	reg("POST /admin/nodes/{name}/runtime/logs", s.cors(s.adminAuth(s.handleNodeRuntimeLogs)))
	// Deliberately no s.adminAuth: the caller is the Marbor Agent process
	// itself during install (agent service install --enroll=<code>), not an
	// authenticated admin browser session. Safety rests entirely on the code
	// being a random 128-bit single-use value with a short TTL (see
	// handleEnrollMarborAgent) - the same trust model as a password-reset link.
	reg("POST /admin/agent/enroll", s.cors(s.handleEnrollMarborAgent))
	reg("GET /admin/audit", s.cors(s.adminAuth(s.handleAudit)))
	reg("GET /admin/system-audit", s.cors(s.adminAuth(s.handleSystemAudit)))
	reg("GET /admin/nodes/model-fit", s.cors(s.adminAuth(s.handleModelFit)))
	reg("GET /admin/models/catalog", s.cors(s.adminAuth(s.handleModelCatalog)))
	reg("GET /admin/models/search", s.cors(s.adminAuth(s.handleModelSearch)))
	reg("GET /admin/models/repo", s.cors(s.adminAuth(s.handleModelRepo)))
	reg("GET /admin/favorites", s.cors(s.adminAuth(s.handleFavorites)))
	reg("POST /admin/favorites", s.cors(s.adminAuth(s.handleAddFavorite)))
	// "{modelId...}" (not "{modelId}") deliberately - HF model ids routinely
	// contain "/" (e.g. "org/repo"), same reasoning as
	// DELETE /admin/nodes/{name}/models/{model...} above.
	reg("DELETE /admin/favorites/{modelId...}", s.cors(s.adminAuth(s.handleRemoveFavorite)))

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
	reg("POST /admin/login", s.cors(s.handleAdminLogin))
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
		mux.Handle("/grafana/marbor.json", s.noCache(http.FileServer(http.FS(sub))))
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
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
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
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyUserID, session.UserID))
		next(w, r)
	}
}

// nodeStateToResp builds the fields common to handleNodes and handleNode from
// a single NodeState. Caller must hold n's RLock. Per-endpoint stats (request
// counts, latency, warm-hit ratio) are computed by the caller, not here, since
// handleNode has never included them (preserved to avoid changing its wire shape).
func (s *Server) nodeStateToResp(n *router.NodeState, id string) nodeResp {
	host := ""
	port := 0
	scheme := "http"
	if u, err := url.Parse(n.URL); err == nil {
		host = u.Hostname()
		port, _ = strconv.Atoi(u.Port())
		if u.Scheme != "" {
			scheme = u.Scheme
		}
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

	var warmupState []warmupStateEntry
	for model, info := range s.router.SuppressedWarmupInfo(n.Name) {
		warmupState = append(warmupState, warmupStateEntry{
			Model:  model,
			State:  "suppressed",
			Reason: info.Reason,
			Since:  info.Since.UTC().Format(time.RFC3339),
		})
	}

	return nodeResp{
		ID:                            id,
		Name:                          n.Name,
		Host:                          host,
		Port:                          port,
		Scheme:                        scheme,
		GPUModel:                      n.GPUModel,
		GPUIndices:                    n.DeclaredGPUIndices,
		MaxInFlight:                   n.MaxInFlight,
		TLSFingerprint:                n.TLSFingerprint,
		TLSFingerprintMismatch:        n.AgentTLSMismatch,
		ParallelismType:               n.ParallelismType,
		ParallelismWidth:              n.ParallelismWidth,
		EffectiveRequiredGPUs:         n.EffectiveRequiredGPUs(),
		DetectedParallelismType:       n.DetectedParallelismType,
		DetectedParallelismWidth:      n.DetectedParallelismWidth,
		DetectedGPUGroup:              append([]int(nil), n.DetectedGPUGroup...),
		DetectedSource:                n.DetectedSource,
		DetectedRuntime:               n.DetectedRuntime,
		DetectedEffectiveRequiredGPUs: n.EffectiveDetectedRequiredGPUs(),
		MismatchWarning:               n.MismatchWarning(),
		VRAMTotalMB:                   n.VRAMTotalMB,
		VRAMUsedMB:                    n.VRAMUsedMB,
		VRAMSource:                    n.VRAMSource,
		PowerDrawW:                    n.PowerDrawW,
		Temperature:                   n.Temperature,
		Runtime:                       n.Runtime,
		Health:                        health,
		Draining:                      n.Draining,
		DrainedReason:                 n.DrainedReason,
		PrewarmDisabled:               n.PrewarmDisabled,
		Uptime:                        n.Uptime,
		LoadedModels:                  safeModelInfoSlice(n.LoadedModels),
		WarmupErrors:                  safeStringMap(n.WarmupErrors),
		UnloadErrors:                  safeStringMap(n.UnloadErrors),
		WarmupState:                   warmupState,
		ActiveConns:                   atomic.LoadInt32(&n.ActiveConns),
		HealthHistory:                 hist,
		PendingPrewarmMB:              s.router.PendingPrewarmBytes(n.Name) / (1024 * 1024),
		AgentPresent:                  n.AgentPresent,
		AgentStale:                    n.AgentStale,
		AgentVersion:                  n.AgentVersion,
		FanPercent:                    n.FanPercent,
		CPUPercent:                    n.CPUPercent,
		RAMUsedMB:                     n.RAMUsedMB,
		DiskFreeGB:                    n.DiskFreeGB,
		AgentCapabilities:             n.AgentCapabilities,
		AgentPlatform:                 n.AgentPlatform,
		AgentArchitecture:             n.AgentArchitecture,
		AgentGPUVendor:                n.AgentGPUVendor,
		AgentRuntime:                  n.AgentRuntime,
		AgentNodeID:                   n.AgentNodeID,
		AgentGPUCount:                 n.AgentGPUCount,
		AgentGPUs:                     toAgentGPUDevices(n.AgentGPUs),
		DriverVersion:                 n.DriverVersion,
		CUDAVersion:                   n.CUDAVersion,
		RAMTotalMB:                    n.RAMTotalMB,
		DiskTotalGB:                   n.DiskTotalGB,
		Hostname:                      n.Hostname,
		UptimeSeconds:                 n.UptimeSeconds,
		BootTime:                      n.BootTime,
		RuntimeVersion:                n.RuntimeVersion,
		RuntimeStatus:                 n.RuntimeStatus,
	}
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	nodes := s.router.Nodes()
	out := make([]nodeResp, len(nodes))
	for i, n := range nodes {
		n.RLock()
		resp := s.nodeStateToResp(n, fmt.Sprintf("gpu-%d", i))

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
		resp.RequestsTotal = atomic.LoadInt64(&n.RequestsTotal)
		resp.ColdStarts = coldNode
		resp.TokensTotal = atomic.LoadInt64(&n.TokensTotal)
		resp.AvgLatencyMs = avgLatencyNode
		resp.WarmHitRatio = warmHitRatioNode

		out[i] = resp
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
		out := s.nodeStateToResp(n, fmt.Sprintf("gpu-%d", i))
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
				Name:                  rk.Name,
				Key:                   rk.Key,
				RateLimit:             rk.RateLimit,
				DailyLimit:            rk.DailyLimit,
				MonthlyLimit:          rk.MonthlyLimit,
				DailyUsdCap:           rk.DailyUsdCap,
				MonthlyUsdCap:         rk.MonthlyUsdCap,
				Models:                rk.Models,
				LocalOnly:             rk.LocalOnly,
				AllowLocalDegradation: rk.AllowLocalDegradation,
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
		localOnly := k.LocalOnly
		if s.auth != nil {
			localOnly = s.auth.IsLocalOnly(k.Name)
		}
		allowLocalDegradation := k.AllowLocalDegradation
		if s.auth != nil {
			allowLocalDegradation = s.auth.IsAllowLocalDegradation(k.Name)
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
			Key:                   maskKey(k.Key),
			Created:               created,
			RequestsToday:         today,
			RequestsThisMonth:     month,
			TokensThisMonth:       tokensMonth,
			EstimatedCostUsd:      estimatedCost,
			RateLimit:             rateLimit,
			DailyUsdCap:           dailyUsdCap,
			MonthlyUsdCap:         monthlyUsdCap,
			Status:                status,
			AllowedModels:         models,
			ExpiresAt:             expires,
			LocalOnly:             localOnly,
			AllowLocalDegradation: allowLocalDegradation,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleSpillCounters returns the full spill_counters table fleet-wide: one
// row per (key_name, served_by), where served_by is "local", a cloud
// provider's Name, or "blocked" (a local_only policy rejection). Response
// shape:
//
//	[
//	  {"key_name": "finance", "served_by": "local",  "requests": 1234},
//	  {"key_name": "finance", "served_by": "openai", "requests": 4},
//	  {"key_name": "finance", "served_by": "blocked","requests": 2}
//	]
//
// A per-key drill-down is a client-side filter of this same payload - the
// table is bounded by keys x providers, so a second endpoint would be pure
// duplication. GET /admin/spill
func (s *Server) handleSpillCounters(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.SpillCounters()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to read spill counters")
		return
	}
	if rows == nil {
		rows = []store.SpillCounterRow{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rows)
}

func (s *Server) handleLiveRequests(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	reqs := make([]RequestLog, len(s.requests))
	copy(reqs, s.requests)
	s.mu.RUnlock()
	// s.requests is append-ordered (oldest first); the dashboard widget takes
	// the first N of whatever this returns, so newest-first here is what
	// makes "latest N requests, updating as new ones arrive" true.
	for i, j := 0, len(reqs)-1; i < j; i, j = i+1, j-1 {
		reqs[i], reqs[j] = reqs[j], reqs[i]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reqs)
}

// handleRequests returns the request log in RequestEntry format for the dashboard.
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		ID            string    `json:"id"`
		Time          time.Time `json:"time"`
		KeyName       string    `json:"key_name"`
		SourceIP      string    `json:"source_ip"`
		Model         string    `json:"model"`
		Node          string    `json:"node"`
		Status        int       `json:"status"`
		LatencyMs     int       `json:"latency_ms"`
		Cloud         bool      `json:"cloud"`
		RoutingReason string    `json:"routingReason,omitempty"`
	}
	s.mu.RLock()
	reqs := make([]RequestLog, len(s.requests))
	copy(reqs, s.requests)
	s.mu.RUnlock()

	out := make([]entry, len(reqs))
	for i, req := range reqs {
		// req.HTTPStatus is the real numeric HTTP status the client received
		// (see LogRequest) - req.Status is a separate semantic label ("warm",
		// "loading", "error", "aborted", "cloud") used for cold/warm tracking
		// and the /admin/requests/live badge, not a status code.
		// Cloud nodes are stored as "cloud:<name>" (e.g. "cloud:openai").
		isCloud := strings.HasPrefix(req.Node, "cloud:")
		out[i] = entry{
			ID:            req.ID,
			Time:          req.Time,
			KeyName:       req.ApiKey,
			SourceIP:      req.SourceIP,
			Model:         req.Model,
			Node:          req.Node,
			Status:        req.HTTPStatus,
			LatencyMs:     req.Latency,
			Cloud:         isCloud,
			RoutingReason: req.RoutingReason,
		}
	}
	// Reverse so newest entries come first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// GET /admin/requests/{id}/explain (also /admin/v1/requests/{id}/explain) -
// P41 per-request routing explainability. Returns the full router.RoutingDecision
// for one request, checking the bounded in-memory ring first (has the fuller
// RoutingDetail with no SQLite round-trip) and falling back to the SQLite
// request_log table for requests that have aged out of the ring but are
// still within its 1000-row retention window. 404 if the id is unknown to
// both, including requests that predate this feature (no decision stored).
func (s *Server) handleExplainRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "request id required")
		return
	}

	var detailJSON string
	s.mu.RLock()
	for _, req := range s.requests {
		if req.ID == id {
			detailJSON = req.RoutingDetail
			break
		}
	}
	s.mu.RUnlock()

	if detailJSON == "" {
		if rec, ok, err := s.st.GetRequest(id); err == nil && ok {
			detailJSON = rec.RoutingDetail
		}
	}

	if detailJSON == "" {
		writeJSONError(w, http.StatusNotFound, "no routing decision recorded for this request id")
		return
	}

	var decision router.RoutingDecision
	if err := json.Unmarshal([]byte(detailJSON), &decision); err != nil {
		log.Printf("admin: handleExplainRequest: unmarshal stored decision for %s: %v", id, err)
		writeJSONError(w, http.StatusInternalServerError, "stored routing decision is corrupt")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(decision)
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
// marbor.db-first replacement for the old "reload config.yaml from disk"
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
			Name:                  k.Name,
			Key:                   k.Key,
			RateLimit:             k.RateLimit,
			DailyLimit:            k.DailyLimit,
			MonthlyLimit:          k.MonthlyLimit,
			DailyUsdCap:           k.DailyUsdCap,
			MonthlyUsdCap:         k.MonthlyUsdCap,
			Models:                k.Models,
			ExpiresAt:             k.ExpiresAt,
			LocalOnly:             k.LocalOnly,
			AllowLocalDegradation: k.AllowLocalDegradation,
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
	// Determine create-vs-update BEFORE calling AddNode, which now upserts
	// by name in place (see Router.AddNode) rather than always appending -
	// this is purely for an honest response status, the router behavior is
	// identical either way.
	isUpdate := false
	for _, existing := range s.router.Nodes() {
		if existing.Name == cfg.Name {
			isUpdate = true
			break
		}
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
		Host:        cfg.Host,
		Runtime:     cfg.Runtime,
		VRAMTotalMB: vramPtr,
	})
	action := "add_node"
	if isUpdate {
		action = "update_node"
	}
	s.logSystemChange(r, action, cfg.Name, fmt.Sprintf("URL: %s, Runtime: %s, VRAM: %dMB", cfg.URL, cfg.Runtime, cfg.VRAMTotalMB))
	if isUpdate {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
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
	_ = s.st.DeleteNode(name)                    // cascades marbor_agent deletion, see sqliteStore.DeleteNode
	_ = s.st.SetSetting("warmup:node:"+name, "") // drop any warmup setting for the node
	s.logSystemChange(r, "remove_node", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// marborAgentInstallCommand returns the one-line commands an operator runs on
// the GPU node to download the binary (if not already present) AND register
// it as a persistent, auto-restarting OS service - install.sh/install.ps1's
// ROLE=agent path (see .local/specs/node-agent.md section 12), which
// downloads the binary then hands off to its own "marbor-agent service
// install" self-registration subcommand (internal/marboragent/service). unix
// covers Linux/macOS; windows is the PowerShell equivalent for Windows
// nodes, since a POSIX sh script can't run there. Safe to re-run for an
// upgrade or to rotate the token - install.sh/service install are both
// idempotent.
//
// The command carries a short-lived, single-use enrollment code, never the
// real permanent token (P50) - a copy-pasted command otherwise leaves the
// real bearer token in shell history/SSH logs/chat forever. marborBaseURL
// tells the agent where to exchange the code for the real token via
// POST /admin/agent/enroll.
func marborAgentInstallCommand(marborBaseURL string, port int, enrollCode string) (unix string, windows string) {
	unix = fmt.Sprintf(
		"curl -fsSL https://raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | ROLE=agent MARBOR_SERVER=%s MARBOR_ENROLL=%s PORT=%d sh",
		marborBaseURL, enrollCode, port,
	)
	windows = fmt.Sprintf(
		`$env:ROLE="agent"; $env:MARBOR_SERVER="%s"; $env:MARBOR_ENROLL="%s"; $env:PORT="%d"; irm https://raw.githubusercontent.com/Anirudhx7/marbor/main/install.ps1 | iex`,
		marborBaseURL, enrollCode, port,
	)
	return unix, windows
}

// requestBaseURL derives the marbor's own address as reachable from wherever r
// came from - the same host:port the operator's browser just used to load
// the admin dashboard, which is the best available guess for what a GPU
// node on the same network can reach back to. Marbor has no separate
// "public URL" setting; this is the first feature that needs marbor to
// know its own address (P50's enrollment exchange).
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// generateMarborAgentToken returns a 32-random-byte, base64url-encoded opaque
// token (per .local/specs/node-agent.md section 5 - a distinct protocol
// from the client-facing API-key mechanism, not a reuse of it), prefixed
// with "<scope>." (P54: per-action token scoping -
// .local/specs/node-agent-capabilities.md section 7). The agent has no DB
// access, only the bare token string it's configured with, so the scope
// travels embedded in the token itself and is parsed agent-side by
// marboragent.scopeOf/TokenScope. scope must be one of marboragent.ScopeReadonly/
// ScopeOperator/ScopeAdmin.
func generateMarborAgentToken(scope string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return scope + "." + base64.RawURLEncoding.EncodeToString(b), nil
}

// generateEnrollmentCode returns a short, URL-safe, single-use code used to
// exchange for the real Marbor Agent token via POST /admin/agent/enroll (P50).
// Deliberately shorter than generateMarborAgentToken: its value as a secret is
// bounded by enrollmentCodeTTL and single-use consumption, not by matching a
// permanent bearer token's entropy - 128 bits is unguessable within a
// 20-minute single-use window regardless.
func generateEnrollmentCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// handleGetMarborAgent returns the current Marbor Agent configuration for a
// node, without the token (the token is only ever returned by the
// enable/regenerate endpoints, at the moment an operator needs to copy it
// into the install command).
// GET /admin/nodes/{name}/agent
func (s *Server) handleGetMarborAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host, found := s.router.NodeHost(name)
	if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	// marbor_agent is keyed by the shared host string (see SetMarborAgent's doc
	// comment) - every node on this host reads/writes the same record.
	rec, found, err := s.st.GetMarborAgent(host)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to read marbor agent config")
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
		"node": name, "enabled": rec.Enabled, "port": rec.Port, "scope": rec.Scope, "scheme": rec.Scheme,
	})
}

// handleEnableMarborAgent enables (or reconfigures) the Marbor Agent for a node:
// generates a fresh token, persists {enabled, port, token, scheme}, pushes
// the config to the live router so polling starts on the next cycle without
// a restart, and returns the one-line install command with the token
// embedded - the only response that ever carries the plaintext token.
// scheme is the AGENT's own transport scheme, entirely independent of this
// node's runtime URL scheme (store.MarborAgentRecord.Scheme's doc comment). An
// omitted scheme means "keep whatever this host's Agent is already
// configured with" on a reconfigure (read from the router's live config,
// not the store - see the no-downgrade lookup below) - it defaults to
// "http" only when there is no existing config at all (first-time enable).
// A caller that wants to actually change the scheme must say so explicitly.
// POST /admin/nodes/{name}/agent  body: {"port": <int>, "scheme"?: "http"|"https"}
func (s *Server) handleEnableMarborAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host, found := s.router.NodeHost(name)
	if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	var body struct {
		Port int `json:"port"`
		// Scheme is a pointer so an omitted field (nil) is distinguishable
		// from an explicit "" - omitted must mean "keep whatever this host's
		// Agent is already configured with" on a reconfigure (e.g. a caller
		// rotating the port/token via this same endpoint), never a silent
		// reset to "http". Only a brand-new host (no existing record) treats
		// omitted as "http" by default.
		Scheme *string `json:"scheme"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Port <= 0 || body.Port > 65535 {
		writeJSONError(w, http.StatusBadRequest, "port must be between 1 and 65535")
		return
	}
	if body.Scheme != nil && *body.Scheme != "http" && *body.Scheme != "https" {
		writeJSONError(w, http.StatusBadRequest, `scheme must be "http" or "https"`)
		return
	}

	// nodePatchMu: this is the same class of check-then-mutate sequence
	// handlePatchNode's TLS validation serializes against (see that field's
	// doc comment above) - the no-downgrade check below and the persist a
	// few lines later must not interleave with a concurrent request (e.g. a
	// PATCH pinning a fingerprint racing this POST reconfiguring the Agent
	// back to http://).
	s.nodePatchMu.Lock()
	defer s.nodePatchMu.Unlock()

	// The router's live in-memory config, not a store read, is the source
	// of truth for "existing" here (State Hierarchy: live beats persisted,
	// guards-detail.md) - it reflects exactly what the poller/action paths
	// are using right now, including in tests that call r.SetMarborAgent
	// directly without a store round-trip.
	scheme := "http"
	if existing, hasExisting := s.router.MarborAgentSetting(name); hasExisting && existing.Scheme != "" {
		scheme = existing.Scheme
	}
	if body.Scheme != nil {
		scheme = *body.Scheme
	}

	// P24 no-downgrade (section 7), moved here from validateTLSPatch's old
	// node.URL-based check now that a pinned fingerprint describes the
	// Agent's own scheme, not the runtime's: reconfiguring the Agent back to
	// http:// while any node sharing this host still has a pinned
	// fingerprint would silently strand that pin (the next poll would fail
	// closed with a confusing mismatch instead of the operator getting a
	// clear, actionable error now). Clear every sibling's pin first (PATCH
	// tls_fingerprint: null) if a genuine downgrade is intended.
	if scheme == "http" {
		for _, n := range s.router.Nodes() {
			n.RLock()
			sameHost, fp := n.Host == host, n.TLSFingerprint
			n.RUnlock()
			if sameHost && fp != "" {
				writeJSONError(w, http.StatusConflict, fmt.Sprintf("marbor agent host %q has a pinned TLS fingerprint (node %q) - clear it first (PATCH /admin/nodes/%s with tls_fingerprint: null) before switching the Agent back to http://", host, n.Name, n.Name))
				return
			}
		}
	}
	token, err := generateMarborAgentToken(marboragent.ScopeAdmin)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	// Persisted/pushed keyed by host, not name - every node sharing this
	// physical machine now reads the same enabled/port/token/scheme record
	// and is polled by the same single agent process (see SetMarborAgent's doc
	// comment). Enabling from any one node's UI panel enables it for all of
	// them.
	rec := store.MarborAgentRecord{Name: host, Enabled: true, Port: body.Port, Token: token, Scope: marboragent.ScopeAdmin, Scheme: scheme}
	if err := s.st.UpsertMarborAgent(rec); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to persist marbor agent config")
		return
	}
	s.router.SetMarborAgent(host, true, body.Port, token, scheme)
	s.logSystemChange(r, "enable_marbor_agent", host, fmt.Sprintf("Port: %d, Scheme: %s", body.Port, scheme))
	code, err := s.newEnrollmentCode(host, token)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to generate enrollment code")
		return
	}
	unixCmd, windowsCmd := marborAgentInstallCommand(requestBaseURL(r), body.Port, code)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"node":                    name,
		"enabled":                 true,
		"port":                    body.Port,
		"scheme":                  scheme,
		"token":                   token,
		"install_command":         unixCmd,
		"install_command_windows": windowsCmd,
	})
}

// handleDisableMarborAgent disables and deletes the Marbor Agent config for a
// node - the router stops polling it on the next cycle (pollAgentTelemetry's
// "no agent configured" branch clears any previously-reported fields). Also
// clears any pinned TLS fingerprint on every node sharing this host: with no
// Agent config left, nothing ever dials it again, so a pin left in place
// would sit inert - looking like an active protection in the UI/API while
// enforcing nothing - until re-enabled, at which point it would silently
// resurrect against whatever cert the Agent (re-)presents. Clearing on
// disable keeps "pinned" always meaning "currently enforced."
// DELETE /admin/nodes/{name}/agent
func (s *Server) handleDisableMarborAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host, found := s.router.NodeHost(name)
	if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}

	// Same nodePatchMu discipline as handleEnableMarborAgent/handlePatchNode -
	// the fingerprint clear below and the agent-config delete must not
	// interleave with a concurrent PATCH re-pinning one of this host's nodes.
	s.nodePatchMu.Lock()
	defer s.nodePatchMu.Unlock()

	empty := ""
	for _, n := range s.router.Nodes() {
		n.RLock()
		sameHost, nodeName, fp := n.Host == host, n.Name, n.TLSFingerprint
		n.RUnlock()
		if sameHost && fp != "" {
			s.router.PatchNode(nodeName, router.NodePatch{TLSFingerprint: &empty})
			if err := s.st.UpsertNodeOverride(nodeName, nil, nil, nil, nil, nil, &empty, nil, nil); err != nil {
				log.Printf("admin: failed to persist cleared TLS fingerprint override for %s: %v", nodeName, err)
			}
		}
	}

	// Disables for the whole shared host, not just this one node row - see
	// SetMarborAgent's doc comment.
	if err := s.st.DeleteMarborAgent(host); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to delete marbor agent config")
		return
	}
	s.router.SetMarborAgent(host, false, 0, "", "")
	s.logSystemChange(r, "disable_marbor_agent", host, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleRegenerateMarborAgentToken issues a fresh token for an already-enabled
// Marbor agent, keeping its configured port. Returns 404 if the agent isn't
// currently enabled for this node (regenerating a token for a disabled/
// nonexistent agent has no meaning - use handleEnableMarborAgent instead).
// POST /admin/nodes/{name}/agent/regenerate
func (s *Server) handleRegenerateMarborAgentToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	host, found := s.router.NodeHost(name)
	if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	rec, found, err := s.st.GetMarborAgent(host)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to read marbor agent config")
		return
	}
	if !found || !rec.Enabled {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("marbor agent not enabled for %q", name))
		return
	}
	token, err := generateMarborAgentToken(marboragent.ScopeAdmin)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	rec.Token = token
	rec.Scope = marboragent.ScopeAdmin
	if err := s.st.UpsertMarborAgent(rec); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to persist marbor agent config")
		return
	}
	s.router.SetMarborAgent(host, true, rec.Port, token, rec.Scheme)
	s.logSystemChange(r, "regenerate_marbor_agent_token", host, "")
	code, err := s.newEnrollmentCode(host, token)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to generate enrollment code")
		return
	}
	unixCmd, windowsCmd := marborAgentInstallCommand(requestBaseURL(r), rec.Port, code)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"node":                    name,
		"port":                    rec.Port,
		"scheme":                  rec.Scheme,
		"token":                   token,
		"install_command":         unixCmd,
		"install_command_windows": windowsCmd,
	})
}

// validControlDrivers are the only driver names an operator may Accept
// (P43 v1 set - marbor-agent-capabilities.md section 5.4). Kept in sync with
// internal/marboragent/control's driver Name() constants.
var validControlDrivers = map[string]bool{
	"systemd": true, "docker": true, "process": true, "launchd": true, "windows_service": true,
}

// handleGetNodeControl returns the node's operator-accepted ControlDriver
// config (from the store) alongside the most recent discovery result (from
// the router's live agent-poll cache) - discovered is never substituted for
// the accepted value (marbor-agent-capabilities.md section 5.6).
// GET /admin/nodes/{name}/control
func (s *Server) handleGetNodeControl(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	n := s.router.FindNode(name)
	if n == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	rec, _, err := s.st.GetNodeControl(name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to read node control config")
		return
	}
	n.RLock()
	discDriver, discIdentifier, discEvidence := n.AgentControlDiscoveredDriver, n.AgentControlDiscoveredIdentifier, n.AgentControlDiscoveredEvidence
	n.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"node":          name,
		"configured":    rec.Configured,
		"driver":        rec.Driver,
		"identifier":    rec.Identifier,
		"start_command": rec.StartCommand,
		"discovered": map[string]interface{}{
			"driver":     discDriver,
			"identifier": discIdentifier,
			"evidence":   discEvidence,
		},
	})
}

// handleAcceptNodeControl persists an operator's explicit Accept of a
// control driver + identifier - the only place `configured` ever changes
// (never as a side effect of a re-scan, section 5.6). driver/identifier are
// typically copied from the discovered block returned by
// handleGetNodeControl, but an operator may type a different value (e.g.
// the Process driver's PID file path is often not auto-discoverable).
// POST /admin/nodes/{name}/control/accept  body: {"driver","identifier"}
func (s *Server) handleAcceptNodeControl(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.router.FindNode(name) == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	var body struct {
		Driver     string `json:"driver"`
		Identifier string `json:"identifier"`
		// StartCommand is only meaningful for the Process driver's Start
		// action (Step 3) - a bare PID-file convention alone gives no way to
		// know how to launch the process fresh. Ignored for every other
		// driver.
		StartCommand string `json:"start_command,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Identifier == "" {
		writeJSONError(w, http.StatusBadRequest, "driver and identifier are required")
		return
	}
	if !validControlDrivers[body.Driver] {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown control driver %q", body.Driver))
		return
	}
	if err := s.st.UpsertNodeControlConfigured(name, body.Driver, body.Identifier, body.StartCommand); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to persist node control config")
		return
	}
	s.router.SetNodeControl(name, router.ControlConfig{Driver: body.Driver, Identifier: body.Identifier, Configured: true, StartCommand: body.StartCommand})
	s.logSystemChange(r, "accept_node_control", name, fmt.Sprintf("Driver: %s, Identifier: %s", body.Driver, body.Identifier))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"node": name, "configured": true, "driver": body.Driver, "identifier": body.Identifier, "start_command": body.StartCommand,
	})
}

// handleClearNodeControl un-configures a node's control driver - lifecycle
// actions on it return "no control driver configured" again until an
// operator accepts a new value. Discovered evidence is left intact (only
// the accepted driver/identifier/configured columns are cleared).
// DELETE /admin/nodes/{name}/control
func (s *Server) handleClearNodeControl(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.router.FindNode(name) == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	if err := s.st.ClearNodeControlConfigured(name); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to clear node control config")
		return
	}
	s.router.SetNodeControl(name, router.ControlConfig{Configured: false})
	s.logSystemChange(r, "clear_node_control", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// nodeRuntimeActionTimeout bounds how long the admin API waits for a node
// agent's runtime start/stop/restart response - generous compared to a
// health check but not as long as a model pull, since a service-manager
// verb (systemctl/docker/launchctl/sc) normally returns quickly even when
// the underlying process takes longer to actually finish starting.
var nodeRuntimeActionTimeout = 30 * time.Second

// handleNodeRuntimeStart/Stop/Restart are the Admin API's dispatch points
// for P43 Step 3's runtime.start/runtime.stop/runtime.restart capabilities -
// POST /admin/nodes/{name}/runtime/{start,stop,restart}. Each follows the
// same template as handleNodeDeleteModel/handleNodeHealthCheck: health
// check -> capability check -> read the operator-accepted ControlDriver
// config from the router (never re-discovered - section 5.6) -> dispatch to
// the agent -> audit log on success.
func (s *Server) handleNodeRuntimeStart(w http.ResponseWriter, r *http.Request) {
	s.handleNodeRuntimeAction(w, r, "start")
}

func (s *Server) handleNodeRuntimeStop(w http.ResponseWriter, r *http.Request) {
	s.handleNodeRuntimeAction(w, r, "stop")
}

func (s *Server) handleNodeRuntimeRestart(w http.ResponseWriter, r *http.Request) {
	s.handleNodeRuntimeAction(w, r, "restart")
}

func (s *Server) handleNodeRuntimeAction(w http.ResponseWriter, r *http.Request, action string) {
	nodeName := r.PathValue("name")

	urls := s.router.NodeURLs()
	nodeURL, ok := urls[nodeName]
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
		return
	}

	// This dispatches to the node's Marbor Agent, not to the runtime itself
	// (start/stop/restart are only meaningful because the runtime may
	// legitimately be down right now) - gate on agent reachability, never
	// nodeIsHealthy's runtime reachability, or "stop" ever succeeding once
	// would permanently block "start" from working again on this node.
	if !marborAgentIsPresent(s.router.Nodes(), nodeName) {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("node %q's agent is currently unreachable - check the marbor agent process/service on that host before running runtime %s", nodeName, action))
		return
	}

	capability := "runtime." + action
	agentCfg, agentOK := s.router.MarborAgentSetting(nodeName)
	if !agentOK || !agentCfg.Enabled || !nodeHasAgentCapability(s.router.Nodes(), nodeName, capability) {
		writeJSONError(w, http.StatusNotImplemented, fmt.Sprintf("node %q has no agent capability for runtime %s", nodeName, action))
		return
	}

	// Marbor constructs {driver, identifier} from its own store-backed
	// cache at dispatch time and hands it to the agent fresh on every
	// request - the agent never persists control config itself (P43 Step 3
	// design decision). A node with no operator-accepted driver returns the
	// exact error marbor-agent-capabilities.md section 5.6 mandates, never a
	// guess.
	ctrl, configured := s.router.NodeControlSetting(nodeName)
	if !configured {
		writeJSONError(w, http.StatusUnprocessableEntity, "Runtime control unavailable: no control driver configured")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), nodeRuntimeActionTimeout)
	defer cancel()
	if err := s.runtimeActionViaAgent(ctx, nodeURL, agentCfg, action, ctrl); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.logSystemChange(r, "runtime_"+action, nodeName, fmt.Sprintf("Driver: %s, Identifier: %s", ctrl.Driver, ctrl.Identifier))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// runtimeActionViaAgent dispatches action ("start"/"stop"/"restart") to
// nodeURL's Marbor Agent (POST /v1/runtime/{action}, capability
// "runtime.{action}"). start_command is only included for the Process
// driver's Start action (Step 2 never persisted a StartCommand for any
// other driver - it stays empty and is simply omitted).
func (s *Server) runtimeActionViaAgent(ctx context.Context, nodeURL string, agentCfg router.MarborAgentConfig, action string, ctrl router.ControlConfig) error {
	actionURL, err := buildAgentURL(nodeURL, agentCfg.Port, agentCfg.Scheme, "/v1/runtime/"+action)
	if err != nil {
		return err
	}

	reqBody := map[string]interface{}{"driver": ctrl.Driver, "identifier": ctrl.Identifier}
	if action == "start" && ctrl.Driver == "process" && ctrl.StartCommand != "" {
		reqBody["start_command"] = ctrl.StartCommand
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentCfg.Token)

	resp, err := s.router.HTTPClientForNode(nodeRuntimeActionTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("agent runtime %s failed: %w", action, err)
	}
	defer resp.Body.Close()

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("agent runtime %s: could not decode response (status %d)", action, resp.StatusCode)
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

// defaultNodeRuntimeLogLines/maxNodeRuntimeLogLines mirror the agent-side
// defaultLogLines/maxLogLines bounds (control_actions.go) - the admin API
// clamps here too so a malformed or hostile ?lines= query can't be forwarded
// straight to the agent unbounded.
const (
	defaultNodeRuntimeLogLines = 200
	maxNodeRuntimeLogLines     = 5000
)

// handleNodeRuntimeLogs is the Admin API's dispatch point for P58's
// runtime.logs capability - POST /admin/nodes/{name}/runtime/logs?lines=N.
// Same template as handleNodeRuntimeAction (health check -> capability
// check -> read the operator-accepted ControlDriver config -> dispatch to
// the agent), but this is a pure read: it never mutates the node, so unlike
// start/stop/restart it is not audit-logged (same reasoning as
// handleNodeHealthCheck, also a pure read with no logSystemChange call).
func (s *Server) handleNodeRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("name")

	urls := s.router.NodeURLs()
	nodeURL, ok := urls[nodeName]
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
		return
	}

	// Same reasoning as handleNodeRuntimeAction: logs are read via the Node
	// Agent, and are exactly what an operator needs while the runtime itself
	// is stopped - gate on agent reachability, not runtime reachability.
	if !marborAgentIsPresent(s.router.Nodes(), nodeName) {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("node %q's agent is currently unreachable - check the marbor agent process/service on that host before running runtime logs", nodeName))
		return
	}

	agentCfg, agentOK := s.router.MarborAgentSetting(nodeName)
	if !agentOK || !agentCfg.Enabled || !nodeHasAgentCapability(s.router.Nodes(), nodeName, "runtime.logs") {
		writeJSONError(w, http.StatusNotImplemented, fmt.Sprintf("node %q has no agent capability for runtime logs", nodeName))
		return
	}

	ctrl, configured := s.router.NodeControlSetting(nodeName)
	if !configured {
		writeJSONError(w, http.StatusUnprocessableEntity, "Runtime control unavailable: no control driver configured")
		return
	}

	lines := defaultNodeRuntimeLogLines
	if raw := r.URL.Query().Get("lines"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			lines = n
		}
	}
	if lines > maxNodeRuntimeLogLines {
		lines = maxNodeRuntimeLogLines
	}

	ctx, cancel := context.WithTimeout(r.Context(), nodeRuntimeActionTimeout)
	defer cancel()
	logLines, err := s.runtimeLogsViaAgent(ctx, nodeURL, agentCfg, ctrl, lines)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"lines": logLines})
}

// runtimeLogsViaAgent dispatches to nodeURL's Marbor Agent (POST
// /v1/runtime/logs, capability "runtime.logs"). start_command is included
// only for the Process driver, same conditional as runtimeActionViaAgent's
// Start case, even though ProcessDriver.Logs ignores it today - keeps the
// payload construction identical if that ever changes.
func (s *Server) runtimeLogsViaAgent(ctx context.Context, nodeURL string, agentCfg router.MarborAgentConfig, ctrl router.ControlConfig, lines int) ([]string, error) {
	actionURL, err := buildAgentURL(nodeURL, agentCfg.Port, agentCfg.Scheme, "/v1/runtime/logs")
	if err != nil {
		return nil, err
	}

	reqBody := map[string]interface{}{"driver": ctrl.Driver, "identifier": ctrl.Identifier, "lines": lines}
	if ctrl.Driver == "process" && ctrl.StartCommand != "" {
		reqBody["start_command"] = ctrl.StartCommand
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentCfg.Token)

	resp, err := s.router.HTTPClientForNode(nodeRuntimeActionTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent runtime logs failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Lines []string `json:"lines"`
		Error string   `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agent runtime logs: could not decode response (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("agent returned %d", resp.StatusCode)
		}
		return nil, errors.New(msg)
	}
	return out.Lines, nil
}

// newEnrollmentCode generates a fresh one-time enrollment code for node,
// wraps the already-generated real token, and stores it in-memory with an
// enrollmentCodeTTL expiry (P50). The map holds only ephemeral state and is
// never persisted - losing an in-flight code on a marbor restart just means
// the operator re-generates it from the admin UI.
func (s *Server) newEnrollmentCode(node, token string) (string, error) {
	code, err := generateEnrollmentCode()
	if err != nil {
		return "", err
	}
	s.enrollMu.Lock()
	s.enrollCodes[code] = enrollmentCode{node: node, token: token, expiresAt: time.Now().Add(enrollmentCodeTTL)}
	s.enrollMu.Unlock()
	return code, nil
}

// handleEnrollMarborAgent exchanges a short-lived, single-use enrollment code
// for the node's real, permanent Marbor Agent bearer token (P50). Called by
// the Marbor Agent itself during "agent service install --enroll=<code>",
// never by an authenticated admin browser session - see the route
// registration comment for why this deliberately skips s.adminAuth. The
// code is deleted from the map unconditionally on lookup, before any
// validation, so a code can never be redeemed twice even under a concurrent
// replay: the second racer's lookup simply misses.
// POST /admin/agent/enroll  body: {"code": "<enrollment code>"}
func (s *Server) handleEnrollMarborAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Code == "" {
		writeJSONError(w, http.StatusBadRequest, "code is required")
		return
	}
	s.enrollMu.Lock()
	rec, found := s.enrollCodes[body.Code]
	if found {
		delete(s.enrollCodes, body.Code)
	}
	s.enrollMu.Unlock()
	if !found || time.Now().After(rec.expiresAt) {
		writeJSONError(w, http.StatusUnauthorized, "invalid or expired enrollment code")
		return
	}
	s.logSystemChange(r, "enroll_marbor_agent", rec.node, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"token": rec.token})
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

// handleFavorites returns the calling user's starred model ids.
// GET /admin/favorites.
func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(ctxKeyUserID).(int64)
	if !ok || userID <= 0 {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ids, err := s.st.ListFavorites(userID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		ModelIDs []string `json:"model_ids"`
	}{ModelIDs: ids})
}

// handleAddFavorite stars a model for the calling user.
// POST /admin/favorites.
func (s *Server) handleAddFavorite(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(ctxKeyUserID).(int64)
	if !ok || userID <= 0 {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ModelID == "" {
		writeJSONError(w, http.StatusBadRequest, "model_id is required")
		return
	}
	if err := s.st.AddFavorite(userID, body.ModelID); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.logSystemChange(r, "add_favorite", body.ModelID, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveFavorite unstars a model for the calling user.
// DELETE /admin/favorites/{modelId...}.
func (s *Server) handleRemoveFavorite(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(ctxKeyUserID).(int64)
	if !ok || userID <= 0 {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	modelID := r.PathValue("modelId")
	if modelID == "" {
		writeJSONError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	if err := s.st.RemoveFavorite(userID, modelID); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.logSystemChange(r, "remove_favorite", modelID, "")
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

// handleUnloadModel evicts a single model from a node's VRAM on operator
// request. It frees VRAM immediately without draining the node or waiting
// for LRU pressure - the manual counterpart to auto-eviction. Dispatches
// through the node's Marbor Agent (capability "models.unload") when available,
// mirroring handleNodePull's dual-path shape - not handleNodeDeleteModel's
// fallback-less 501 shape, since unload already has a legitimate direct
// fallback today (Ollama's own keep_alive:0 HTTP trick, via
// router.UnloadModel) that pull/delete/list never had. The agent path is
// what makes this work for real on non-Ollama runtimes instead of silently
// no-op-ing (R1) - see actions.go's unloadCommands for exactly which
// runtimes that currently covers.
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

	// Node-existence checked first, ahead of the pin check below - same
	// priority order the pre-existing direct-only path had (router.UnloadModel
	// returned 404 before ever consulting pin state). A stale pinned-map entry
	// for a since-removed/renamed node name must not turn into a false 409;
	// it should still 404 like any other unknown node.
	nodeURL, nodeOK := s.router.NodeURLs()[name]
	if !nodeOK {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}

	// Pinning means "never evict/unload without an explicit unpin first" -
	// checked once, ahead of the agent/direct branch, so it is honored on
	// both paths identically rather than only on the direct one (where it
	// used to live inside router.UnloadModel itself). There is no
	// force-override; unpin, then unload.
	if s.router.IsPinned(name, body.Model) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": router.ErrModelPinned.Error()})
		return
	}

	// Decision shared with the scheduled unload path (router.Router.UnloadModels)
	// via Router.ShouldUseAgentForUnload, per P33 - not duplicated a second
	// time here.
	agentCfg, useAgent := s.router.ShouldUseAgentForUnload(name)

	if useAgent {
		// Same fail-fast reasoning as handleNodePull/handleNodeDeleteModel: a
		// down node's URL may still answer with something (another service on
		// that port), producing a confusing "failed to unload model on node:
		// ..." error that looks capability-specific when the real problem is
		// just reachability.
		if !nodeIsHealthy(s.router.Nodes(), name) {
			writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("node %q is currently unreachable (down) - check its URL/connectivity before unloading a model", name))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), nodeUnloadModelTimeout)
		defer cancel()
		ctrl, _ := s.router.NodeControlSetting(name)
		if err := s.unloadModelViaAgent(ctx, nodeURL, agentCfg, body.Model, ctrl); err != nil {
			// The agent's own error text (e.g. "unsupported: no unload
			// primitive for runtime \"vllm\"") is the whole point of R1 here -
			// it is what turns a non-Ollama runtime into a clear "not
			// supported" message in the UI instead of a correlation-id-only
			// generic failure, matching deleteModelViaAgent's/
			// listModelsViaAgent's convention (unlike the pre-existing direct
			// path below, which keeps writeCorrelatedError for its own
			// Ollama-HTTP-level failures).
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		// The agent path never goes through router.unloadModel, which is the
		// only other place that suppresses the next warmupTicker ping and
		// drops the model from warm state - without this, pingWarmupModels
		// would silently reload the model straight back into VRAM on its next
		// tick, undoing the operator's unload.
		s.router.RecordManualUnload(name, body.Model)
	} else {
		found, err := s.router.UnloadModel(r.Context(), name, body.Model)
		if !found {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
			return
		}
		if errors.Is(err, router.ErrModelPinned) {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, router.ErrUnloadUnsupported) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			writeCorrelatedError(w, r, http.StatusBadGateway, "failed to unload model on node", err)
			return
		}
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
	s.scheduleMu.Lock()
	s.persistSchedules(append(s.router.Schedules(), sc))
	s.scheduleMu.Unlock()
	s.logSystemChange(r, "create_schedule", sc.ID, fmt.Sprintf("Action: %s, Node: %s, At: %s, Models: %v, Enabled: %v", sc.Action, sc.Node, sc.At, sc.Models, sc.Enabled))
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
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
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
	s.logSystemChange(r, "patch_schedule", sc.ID, fmt.Sprintf("Action: %s, Node: %s, At: %s, Models: %v, Enabled: %v", sc.Action, sc.Node, sc.At, sc.Models, sc.Enabled))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sc)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
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
	s.logSystemChange(r, "delete_schedule", id, "")
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
	_ = json.NewEncoder(w).Encode(map[string]any{"node": name, "draining": true, "reason": reason})
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
	_ = json.NewEncoder(w).Encode(map[string]any{"node": name, "draining": false})
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
	// This path bypasses config.Validate(), so the same range check applied
	// there to routing.max_in_flight_per_node/node config at boot must be
	// repeated here: negative silently reads as "uncapped" via
	// isUnderCapacity's own <=0 fallback (opposite of an operator's intent to
	// restrict a node), and a value above math.MaxInt32 wraps negative when
	// cast to int32 for the ActiveConns comparison, silently making the node
	// permanently unroutable.
	if patch.MaxInFlight != nil && (*patch.MaxInFlight < 0 || *patch.MaxInFlight > math.MaxInt32) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("max_in_flight must be between 0 (use the global default) and %d", math.MaxInt32))
		return
	}
	if patch.TLSFingerprint != nil && *patch.TLSFingerprint != "" && !isValidTLSFingerprint(*patch.TLSFingerprint) {
		writeJSONError(w, http.StatusBadRequest, "tls_fingerprint must be empty (to clear the pin) or in the form SHA256:<64 hex characters>")
		return
	}
	// P397: parallelism validation - structured type tp|pp|ep|dp + width 1..64.
	if patch.ParallelismType != nil && *patch.ParallelismType != "" {
		switch *patch.ParallelismType {
		case "tp", "pp", "ep", "dp":
		default:
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("parallelism_type must be one of tp, pp, ep, dp (got %q)", *patch.ParallelismType))
			return
		}
	}
	if patch.ParallelismWidth != nil && (*patch.ParallelismWidth < 0 || *patch.ParallelismWidth > 64) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("parallelism_width must be between 0 and 64 (got %d)", *patch.ParallelismWidth))
		return
	}
	if (patch.ParallelismType != nil && *patch.ParallelismType != "") != (patch.ParallelismWidth != nil && *patch.ParallelismWidth != 0) {
		writeJSONError(w, http.StatusBadRequest, "parallelism_type and parallelism_width must be set together or cleared together")
		return
	}
	// Derive resulting gpu_indices for cross-field validation - need existing value when patch doesn't carry new one.
	if patch.ParallelismType != nil && *patch.ParallelismType != "" && patch.ParallelismWidth != nil && *patch.ParallelismWidth > 0 {
		var resultingGPUIndices []int
		if patch.GPUIndices != nil {
			resultingGPUIndices = *patch.GPUIndices
		} else {
			for _, n := range s.router.Nodes() {
				if n.Name == name {
					n.RLock()
					resultingGPUIndices = append([]int(nil), n.DeclaredGPUIndices...)
					n.RUnlock()
					break
				}
			}
		}
		if len(resultingGPUIndices) > 0 && len(resultingGPUIndices) < *patch.ParallelismWidth {
			writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf("parallelism_width %d requires gpu_indices len >=%d got %d", *patch.ParallelismWidth, *patch.ParallelismWidth, len(resultingGPUIndices)))
			return
		}
	}
	// The entire validate-then-mutate transaction below runs under
	// nodePatchMu: two concurrent PATCH requests to DIFFERENT node names can
	// otherwise each read a pre-mutation node-list snapshot inside
	// validateTLSPatch, both pass its sibling-consistency check against that
	// now-stale snapshot, and then both mutate - jointly producing two nodes
	// on one Host with different pinned TLS fingerprints (P24 section 15's
	// invariant, reopened via a race instead of a single request). Holding
	// one mutex across validation and every mutation this handler performs
	// (UpdateNodeURL, PatchNode, and their store-persistence calls) makes
	// the whole transaction atomic with respect to other PATCH requests,
	// without changing any of the per-error status/message logic below.
	locked := func() bool {
		s.nodePatchMu.Lock()
		defer s.nodePatchMu.Unlock()

		// P397: gpu_indices vs existing parallelism width cross-check when only
		// gpu_indices changes (early validation above only covered the case
		// where parallelism fields were in the patch).
		if patch.GPUIndices != nil {
			for _, n := range s.router.Nodes() {
				if n.Name == name {
					n.RLock()
					existingWidth := n.ParallelismWidth
					n.RUnlock()
					if existingWidth > 0 && len(*patch.GPUIndices) > 0 && len(*patch.GPUIndices) < existingWidth {
						writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf("parallelism_width %d requires gpu_indices len >=%d got %d", existingWidth, existingWidth, len(*patch.GPUIndices)))
						return false
					}
					break
				}
			}
		}

		// P24: no-downgrade and section 15 sibling-consistency checks. Must run
		// before any mutation below (UpdateNodeURL/PatchNode) so a rejected
		// patch never partially applies. See .local/specs/node-agent-tls.md
		// sections 5/7/15.
		if patch.TLSFingerprint != nil || patch.URL != nil {
			if err := s.validateTLSPatch(name, patch); err != nil {
				status := http.StatusConflict
				if strings.Contains(err.Error(), "not found") {
					status = http.StatusNotFound
				}
				writeJSONError(w, status, err.Error())
				return false
			}
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
				return false
			}
			_ = s.st.UpdateNodeURL(name, *patch.URL)
		}
		if patch.VRAMTotalMB != nil || patch.GPUModel != nil || patch.Runtime != nil || patch.GPUIndices != nil || patch.MaxInFlight != nil || patch.TLSFingerprint != nil || patch.ParallelismType != nil || patch.ParallelismWidth != nil {
			if !s.router.PatchNode(name, patch) {
				writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
				return false
			}
			if err := s.st.UpsertNodeOverride(name, patch.VRAMTotalMB, patch.GPUModel, patch.Runtime, patch.GPUIndices, patch.MaxInFlight, patch.TLSFingerprint, patch.ParallelismType, patch.ParallelismWidth); err != nil {
				log.Printf("admin: failed to persist node override for %s: %v", name, err)
			}
		}
		return true
	}()
	if !locked {
		return
	}
	s.logSystemChange(r, "patch_node", name, fmt.Sprintf("URLChanged: %v, VRAMTotalMBChanged: %v, GPUModelChanged: %v, RuntimeChanged: %v, GPUIndicesChanged: %v, MaxInFlightChanged: %v, TLSFingerprintChanged: %v, ParallelismChanged: %v", patch.URL != nil, patch.VRAMTotalMB != nil, patch.GPUModel != nil, patch.Runtime != nil, patch.GPUIndices != nil, patch.MaxInFlight != nil, patch.TLSFingerprint != nil, patch.ParallelismType != nil || patch.ParallelismWidth != nil))
	// Return the updated node.
	s.handleNode(w, r)
}

// isValidTLSFingerprint reports whether s is a well-formed pinned Node
// Agent cert fingerprint: "SHA256:" followed by exactly 64 hex characters
// (a SHA-256 digest), matching router.CertFingerprintSHA256's exact output
// format (no byte-separator colons). Callers must check for the "clear the
// pin" empty-string case separately - this only validates a non-empty value.
func isValidTLSFingerprint(s string) bool {
	const prefix = "SHA256:"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	hexPart := s[len(prefix):]
	if len(hexPart) != 64 {
		return false
	}
	for _, c := range hexPart {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// validateTLSPatch enforces P24's no-downgrade rule (section 7) and section
// 15's multi-GPU-per-host sibling-consistency invariant, computed against
// the RESULTING state a patch would produce (current node state merged with
// whichever of URL/TLSFingerprint this patch actually sets), before any
// mutation happens. Returns an error whose message contains "not found" if
// name does not exist (handlePatchNode maps that to 404, matching every
// other not-found path in this handler).
func (s *Server) validateTLSPatch(name string, patch router.NodePatch) error {
	nodes := s.router.Nodes()
	var target *router.NodeState
	for _, n := range nodes {
		if n.Name == name {
			target = n
			break
		}
	}
	if target == nil {
		return fmt.Errorf("node %q not found", name)
	}

	target.RLock()
	currentURL := target.URL
	currentFP := target.TLSFingerprint
	host := target.Host
	target.RUnlock()

	resultingURL := currentURL
	if patch.URL != nil {
		resultingURL = *patch.URL
	}
	resultingFP := currentFP
	if patch.TLSFingerprint != nil {
		resultingFP = *patch.TLSFingerprint
	}
	// resultingHost is the host whose Marbor Agent this patch's resulting
	// state actually describes - a URL-only patch can move a node onto a
	// completely different host (with a different, or no, Agent
	// configured), so this must be looked up by the RESULTING host, never
	// by name (which only ever resolves to the node's CURRENT, pre-patch
	// host - see MarborAgentSettingByHost's doc comment).
	resultingHost := router.ResultingHost(host, currentURL, resultingURL)

	// No-downgrade (section 7): once a fingerprint is pinned, marbor must
	// never end up treating the Marbor Agent as plaintext without an explicit
	// clear (tls_fingerprint: null/""). This checks the AGENT's own
	// configured scheme for the RESULTING host (POST /admin/nodes/{name}/agent's
	// scheme field, see store.MarborAgentRecord.Scheme's doc comment) - NOT
	// the node's runtime URL scheme, which handlePatchNode's patch.URL
	// controls and which is entirely independent of the Agent's transport.
	// A pinned fingerprint always describes the Agent's TLS certificate,
	// never the runtime's.
	if resultingFP != "" {
		agentCfg, hasAgent := s.router.MarborAgentSettingByHost(resultingHost)
		if !hasAgent || !agentCfg.Enabled || agentCfg.Scheme != "https" {
			return fmt.Errorf("node %q would have a pinned TLS fingerprint but its marbor agent (host %q) is not configured for https:// - enable Agent HTTPS (POST /admin/nodes/%s/agent with scheme:\"https\") before pinning, or clear the pin (tls_fingerprint: null)", name, resultingHost, name)
		}
	}

	// Section 15: multi-GPU-per-host sibling consistency. Every NodeState
	// sharing this node's Host talks to the exact same physical Marbor Agent
	// process/certificate, so they may only ever agree on one pinned
	// fingerprint (or none) - never disagree. Identical pins across siblings
	// are fine and expected; this rejects only a genuine conflict. Storage
	// stays per-node-name exactly as the frozen spec's sections 3/4
	// specify - this check does not redesign it to host-level storage, it
	// only prevents siblings from drifting apart.
	//
	// Gated on resultingFP/resultingHost (the state this patch would
	// actually produce), not on the raw patch fields: a URL-only PATCH
	// (patch.TLSFingerprint == nil) carries the node's EXISTING pin into
	// whatever new host the URL points at, via UpdateNodeURL's
	// TLSFingerprint carry-through - that resulting pin can conflict with a
	// sibling on the destination host exactly as much as an explicit
	// tls_fingerprint patch can, so it must be checked the same way. Gating
	// on patch.TLSFingerprint alone (this function's original condition)
	// missed that case entirely: a pinned node moved by URL alone onto a
	// host with a different pinned sibling would pass validation, write the
	// conflicting pair to storage, and only be caught afterward by
	// dialTLSContext's ambiguity check (tls_dial.go) - which fails closed,
	// but for BOTH siblings, since it cannot tell which pin is correct. This
	// check exists to reject that mutation before it is ever persisted.
	if resultingFP != "" {
		for _, n := range nodes {
			if n.Name == name {
				continue
			}
			n.RLock()
			sameHost := n.Host == resultingHost
			siblingFP := n.TLSFingerprint
			n.RUnlock()
			if sameHost && siblingFP != "" && siblingFP != resultingFP {
				return fmt.Errorf("node %q would share Marbor Agent host %q with node %q, which is already pinned to a different fingerprint - every node sharing one physical Marbor Agent must share the same pin (see .local/specs/node-agent-tls.md section 15)", name, resultingHost, n.Name)
			}
		}
	}

	return nil
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
func generateAPIKey(name string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	slug := apiKeyNameSlugRe.ReplaceAllString(strings.ToLower(name), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "key"
	}
	return "sk-" + slug + "-" + hex.EncodeToString(b), nil
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
				s.logLoginAttempt(r, req.Username, false)
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"not an admin account"}`))
				return
			}
			if s.loginLimiter != nil {
				s.loginLimiter.recordSuccess(ip)
			}
			s.logLoginAttempt(r, "admin", true)
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
		s.logLoginAttempt(r, req.Username, false)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}

	user, err := s.st.GetUserByUsername(req.Username)
	if err != nil {
		if s.loginLimiter != nil {
			s.loginLimiter.recordFailure(ip)
		}
		s.logLoginAttempt(r, req.Username, false)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}

	// Password verified before role/status are checked, and every rejection
	// past this point returns the same generic 401 "invalid credentials" (P128):
	// an unauthenticated caller must not be able to distinguish "wrong
	// password" from "right password, wrong role" or "right password, pending/
	// suspended account" - either distinction is a username/role enumeration
	// oracle, and every branch here now throttles via recordFailure like any
	// other rejected attempt.
	if !verifyPassword(user.PasswordHash, req.Password) {
		if s.loginLimiter != nil {
			s.loginLimiter.recordFailure(ip)
		}
		s.logLoginAttempt(r, req.Username, false)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}

	if requiredRole != "" && user.Role != requiredRole {
		if s.loginLimiter != nil {
			s.loginLimiter.recordFailure(ip)
		}
		s.logLoginAttempt(r, req.Username, false)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}

	switch user.Status {
	case "pending", "suspended":
		if s.loginLimiter != nil {
			s.loginLimiter.recordFailure(ip)
		}
		s.logLoginAttempt(r, req.Username, false)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}

	if s.loginLimiter != nil {
		s.loginLimiter.recordSuccess(ip)
	}
	s.logLoginAttempt(r, user.Username, true)

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
	if _, err := rand.Read(b); err != nil {
		writeServerError(w, r, err)
		return
	}
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
	if _, err := rand.Read(b); err != nil {
		writeServerError(w, r, err)
		return
	}
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
		var err error
		newKeyValue, err = generateAPIKey(keyName)
		if err != nil {
			writeServerError(w, r, err)
			return
		}
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
	var ip string
	if s.resetPwLimiter != nil {
		ip = clientIP(r)
		if ok, retryAfter := s.resetPwLimiter.allow(ip); !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
			writeJSONError(w, http.StatusTooManyRequests, "too many password resets, try again later")
			return
		}
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		// Only count reset-limiter failures for validated targets - a
		// malformed id is a caller/client-side error, not evidence of a
		// brute-force target-enumeration attempt worth throttling toward.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid user id"}`))
		return
	}
	user, err := s.st.GetUserByID(id)
	if err != nil {
		if s.resetPwLimiter != nil {
			s.resetPwLimiter.recordFailure(ip)
		}
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
	if s.resetPwLimiter != nil {
		s.resetPwLimiter.recordSuccess(ip)
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
		key, err := generateAPIKey(k.Name)
		if err != nil {
			writeServerError(w, r, err)
			return
		}
		k.Key = key
	}
	if s.auth != nil {
		s.auth.AddKey(k)
	}
	_ = s.st.UpsertKey(store.KeyRecord{
		Name:                  k.Name,
		Key:                   k.Key,
		RateLimit:             k.RateLimit,
		DailyLimit:            k.DailyLimit,
		MonthlyLimit:          k.MonthlyLimit,
		DailyUsdCap:           k.DailyUsdCap,
		MonthlyUsdCap:         k.MonthlyUsdCap,
		Models:                k.Models,
		Revoked:               false,
		ExpiresAt:             k.ExpiresAt,
		LocalOnly:             k.LocalOnly,
		AllowLocalDegradation: k.AllowLocalDegradation,
	})
	s.logSystemChange(r, "add_key", k.Name, fmt.Sprintf("RateLimit: %d, DailyLimit: %d, MonthlyLimit: %d, DailyUsdCap: %f, MonthlyUsdCap: %f, Models: %v, LocalOnly: %v, AllowLocalDegradation: %v", k.RateLimit, k.DailyLimit, k.MonthlyLimit, k.DailyUsdCap, k.MonthlyUsdCap, k.Models, k.LocalOnly, k.AllowLocalDegradation))
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
	// Persist the patch changes to SQLite. Note: keyRecord stays nil (a
	// deliberate no-op, not an error) when running with no store attached
	// (NewServer's NopStore fallback, e.g. cmd/demo) - the in-memory
	// auth.PatchKey above still applies in that mode.
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
		if patch.LocalOnly != nil {
			keyRecord.LocalOnly = *patch.LocalOnly
		}
		if patch.AllowLocalDegradation != nil {
			keyRecord.AllowLocalDegradation = *patch.AllowLocalDegradation
		}
		_ = s.st.UpsertKey(*keyRecord)
	}
	s.logSystemChange(r, "patch_key", name, fmt.Sprintf("RateLimitChanged: %v, DailyLimitChanged: %v, MonthlyLimitChanged: %v, DailyUsdCapChanged: %v, MonthlyUsdCapChanged: %v, ModelsChanged: %v, ExpiresAtChanged: %v, LocalOnlyChanged: %v, AllowLocalDegradationChanged: %v", patch.RateLimit != nil, patch.DailyLimit != nil, patch.MonthlyLimit != nil, patch.DailyUsdCap != nil, patch.MonthlyUsdCap != nil, patch.Models != nil, patch.ExpiresAt != nil, patch.LocalOnly != nil, patch.AllowLocalDegradation != nil))
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

	// Per-user display timezone - so one user's Settings change doesn't affect
	// another user's wall-clock rendering. The global scheduler timezone is
	// stored under "timezone" and drives s.router.localNow for fleet-wide
	// scheduling, but the UI's TimezoneProvider prefers the per-user value
	// returned here. If the user has never set a personal timezone, seed from
	// the global scheduler value (if any) so an existing fleet's prior global
	// choice becomes each user's initial per-user display value; after seeding,
	// later global changes won't affect that user's display (per-user isolation).
	if val, err := s.st.GetSetting("pref:" + username + ":timezone"); err == nil && val != "" {
		cfg.Timezone = val
	} else if gv, err := s.st.GetSetting("timezone"); err == nil && gv != "" && gv != "Local" {
		cfg.Timezone = gv
		_ = s.st.SetSetting("pref:"+username+":timezone", gv)
	} else {
		cfg.Timezone = "Local"
	}

	// Backup.LastBackupAt/LastBackupError are read-only status, never stored
	// on cfg itself - overlay the live in-memory state (seeded from the
	// backup_last_at/backup_last_error settings on LoadFromStore, updated by
	// every backup attempt via recordBackupResult).
	s.backupMu.Lock()
	if !s.lastBackupAt.IsZero() {
		cfg.Backup.LastBackupAt = s.lastBackupAt.UTC().Format(time.RFC3339)
	}
	cfg.Backup.LastBackupError = s.lastBackupErr
	s.backupMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	// s.mu also guards cors() (every admin request) and LogRequest (every
	// proxied inference request) - decode the body before taking the lock so
	// a slow/large client body can't stall the entire admin UI and data plane.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	s.mu.RLock()
	// Snapshot s.cfg as JSON bytes rather than a struct copy: Config has
	// several map/slice fields (ContextWindows, Routing.LocalDegradationChains,
	// Nodes, CloudProviders, ...) whose headers a plain `incoming := s.cfg`
	// copies by reference - decoding the request body into that shallow copy
	// with no lock held would then write straight into the SAME backing
	// map/slice s.cfg still points to, racing any concurrent reader (a
	// concurrent map write while a reader ranges the same map panics the
	// whole process, not just a logic bug). Marshal-then-unmarshal into two
	// independent structs below gives incoming/current their own maps and
	// slices with no aliasing at all.
	snapshot, err := json.Marshal(s.cfg)
	s.mu.RUnlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Start from the current config so any field the request body omits (the
	// Settings page only ever sends a partial payload - most Routing/Auth/
	// CloudProviders fields are managed via their own dedicated endpoints)
	// keeps its existing value instead of being silently zeroed. Decoding
	// JSON onto a populated struct only overwrites keys actually present in
	// the body, which also makes explicit "false"/zero values (e.g.
	// disabling routing.session_affinity) apply correctly instead of being
	// mistaken for "unset".
	var incoming config.Config
	if err := json.Unmarshal(snapshot, &incoming); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	var current config.Config
	if err := json.Unmarshal(snapshot, &current); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// The client echoes back the masked "***" placeholder (or omits the field
	// entirely) when the operator didn't change it; preserve the real token in
	// both cases instead of clobbering it with the mask.
	if incoming.HuggingFace.Token == "" || incoming.HuggingFace.Token == "***" {
		incoming.HuggingFace.Token = current.HuggingFace.Token
	}
	if incoming.Webhook.Secret == "" || incoming.Webhook.Secret == "***" {
		incoming.Webhook.Secret = current.Webhook.Secret
	}
	if incoming.LiteLLM.APIKey == "" || incoming.LiteLLM.APIKey == "***" {
		incoming.LiteLLM.APIKey = current.LiteLLM.APIKey
	}

	if err := incoming.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("validation failed: %v", err), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	// Auth.Keys/CloudProviders/Nodes are never part of the Settings page's
	// payload (see the comment above incoming's construction) - they're
	// mutated independently by ReloadFromStore (SIGHUP / POST /admin/config/
	// reload) and handleUpdateCloudProviders, both of which take s.mu.Lock()
	// directly and can run during the window this handler had s.mu released
	// for (body decode/validate). Re-read them fresh right here, under the
	// same lock as the final write, so a concurrent reload's change is never
	// silently reverted by this handler overwriting the whole struct with an
	// older snapshot.
	incoming.Auth.Keys = s.cfg.Auth.Keys
	incoming.CloudProviders = s.cfg.CloudProviders
	incoming.Nodes = s.cfg.Nodes
	s.cfg = incoming
	s.mu.Unlock()

	if s.mgmtEndpoints != nil {
		s.mgmtEndpoints.SetAllowManagementEndpoints(incoming.Routing.AllowManagementEndpoints)
		s.mgmtEndpoints.SetTrustProxyHeaders(incoming.Proxy.TrustProxyHeaders)
	}
	if s.auditLog != nil {
		s.auditLog.SetEnabled(incoming.Audit.Enabled)
	}
	s.router.SetTimezone(incoming.Timezone)
	s.router.SetLiteLLM(incoming.LiteLLM)
	// Per-user display timezone (so one user's change doesn't affect others'
	// wall-clock rendering via handleSettings's per-user override above).
	// We still persist the global "timezone" for the fleet-wide scheduler
	// (router.localNow) so scheduling follows the last admin's choice.
	tzUsername, _ := r.Context().Value(ctxKeyUsername).(string)
	if tzUsername == "" {
		tzUsername = "admin"
	}
	if err := s.st.SetSetting("pref:"+tzUsername+":timezone", incoming.Timezone); err != nil {
		log.Printf("admin: failed to persist per-user timezone setting: %v", err)
	}
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
		"routing_max_in_flight_per_node":                strconv.Itoa(incoming.Routing.MaxInFlightPerNode),
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

		// Scheduled backup (P49). LastBackupAt/LastBackupError are
		// deliberately excluded here - they're read-only status written only
		// by recordBackupResult, never accepted from a client PUT.
		"backup_enabled":         strconv.FormatBool(incoming.Backup.Enabled),
		"backup_interval_hours":  strconv.Itoa(incoming.Backup.IntervalHours),
		"backup_retention_count": strconv.Itoa(incoming.Backup.RetentionCount),
		"backup_target_dir":      incoming.Backup.TargetDir,
	}
	for key, val := range scalarSettings {
		if err := s.st.SetSetting(key, val); err != nil {
			log.Printf("admin: failed to persist %s setting: %v", key, err)
		}
	}

	// List/map-typed fields: JSON-encoded, not representable as a single
	// scalar settings value.
	jsonSettings := map[string]any{
		"warmup_models":                    incoming.Warmup.Models,
		"routing_fallback_chains":          incoming.Routing.FallbackChains,
		"routing_local_degradation_chains": incoming.Routing.LocalDegradationChains,
		"context_windows":                  incoming.ContextWindows,
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

// logLoginAttempt appends a system-audit entry for a login attempt, success
// or failure - previously handleLoginForRole never recorded login activity
// at all, leaving no durable trail of who attempted (or achieved) admin
// access from where. Uses the attempted username directly (not
// logSystemChange's ctxKeyUsername lookup, which is unset pre-auth and would
// otherwise misattribute every failed attempt to "admin").
func (s *Server) logLoginAttempt(r *http.Request, username string, success bool) {
	action := "login_failure"
	if success {
		action = "login_success"
	}
	source := r.RemoteAddr
	if host, _, err := net.SplitHostPort(source); err == nil {
		source = host
	}
	_ = s.st.AppendSystemAuditLog(store.SystemAuditEntry{
		Time:     time.Now(),
		Username: username,
		Action:   action,
		Target:   username,
		SourceIP: source,
	})
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

// IncrSpill increments the (keyName, servedBy) row in spill_counters.
// servedBy is "local", a cloud provider's Name, or "blocked" (a local_only
// policy rejection). Best-effort accounting only: a SQLite error here (e.g.
// momentarily locked) is logged and otherwise ignored - it must never affect
// the inference request that triggered it, the same posture LogRequest's
// own async writer already takes on a full queue.
func (s *Server) IncrSpill(keyName, servedBy string) {
	if err := s.st.IncrSpillCounter(keyName, servedBy); err != nil {
		log.Printf("admin: IncrSpill(%s, %s): %v", keyName, servedBy, err)
	}
}

// LogRequest records a completed request. status is a semantic label
// ("warm", "loading", "error", "aborted", "cloud") used for cold/warm
// tracking and the live-requests dashboard badge - it is never parsed as a
// number. httpStatus is the real numeric HTTP status the client received
// (from statusRecorder.StatusCode() in proxy.go) and is what gets persisted
// to the SQLite request_log and served from /admin/requests.
//
// requestID is the trace ID proxy.go already generates once per request
// (the same one used for X-Request-ID and audit.Entry.RequestID) - it
// becomes request_log.id, replacing a previously independently-minted id.
// This has no external format coupling (verified during P41's
// verify-before-build pass) and lets request_log rows be correlated with
// audit_log/access-log entries for the same request, which the two
// separately-minted ID spaces could not before.
//
// decision is the P41 routing explanation from the router for this
// request's chosen node; nil for cloud-fallback requests, which have no
// router.RoutingDecision.
func (s *Server) LogRequest(requestID, apiKey, sourceIP, model, node, status string, httpStatus int, latencyMs int, tokens int64, decision *router.RoutingDecision) {
	var tps float64
	if tokens > 0 && latencyMs > 0 {
		tps = float64(tokens) / (float64(latencyMs) / 1000.0)
	}
	// Attribute token usage to the calling key for per-key analytics + cost.
	if s.auth != nil {
		s.auth.AddKeyTokens(apiKey, tokens)
	}
	now := time.Now()
	id := requestID
	if id == "" {
		// Defensive fallback for any caller that doesn't have a trace ID
		// (e.g. a direct test call) - never persist an empty primary key.
		b := make([]byte, 4)
		if _, err := rand.Read(b); err == nil {
			id = "req-" + hex.EncodeToString(b)
		} else {
			id = fmt.Sprintf("req-%x", now.UnixNano())
		}
	}
	var routingReason, routingDetail string
	if decision != nil {
		routingReason = decision.Reason
		if detailJSON, err := json.Marshal(decision); err == nil {
			routingDetail = string(detailJSON)
		} else {
			log.Printf("admin: LogRequest: marshal routing decision for %s: %v", id, err)
		}
	}
	s.mu.Lock()
	s.requests = append(s.requests, RequestLog{
		ID:            id,
		ApiKey:        apiKey,
		SourceIP:      sourceIP,
		Model:         model,
		Node:          node,
		Status:        status,
		HTTPStatus:    httpStatus,
		Latency:       latencyMs,
		Tokens:        tokens,
		TokensPerSec:  tps,
		Time:          now,
		RoutingReason: routingReason,
		RoutingDetail: routingDetail,
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

	select {
	case s.logChan <- store.RequestRecord{
		ID:            id,
		KeyName:       apiKey,
		Model:         model,
		NodeName:      node,
		StatusCode:    httpStatus,
		LatencyMs:     int64(latencyMs),
		TokensUsed:    tokens,
		TS:            now,
		RoutingReason: routingReason,
		RoutingDetail: routingDetail,
	}:
	default:
		// Prevent blocking the proxy path if SQLite writes are completely backed up.
		log.Printf("async logger: queue full, dropped request log %s", id)
	}

	// audit_log is NOT written here. proxy.go already calls h.audit.Log
	// directly (once for the local path, once for the cloud path) right after
	// calling LogRequest, using the request's real trace ID and richer data
	// (correct Cloud bool without prefix-guessing, plus CloudModel) - a second
	// write here duplicated every request in the audit_log table under a
	// different, separately-generated ID.
}

// TrackLocalRequestModel tracks a local request with model-level granularity.
// tokens is the real token count parsed from the response (eval_count +
// prompt_eval_count); 0 means the count was unavailable and contributes
// nothing to savings. genDurationMs is Ollama's real eval_duration in
// milliseconds (generation time only, excluding prompt processing); 0 means
// unavailable (cloud responses never report it) and is excluded from the
// hourly tokens-per-second rollup rather than skewing it toward infinity.
func (s *Server) TrackLocalRequestModel(keyName, model string, tokens, genDurationMs int64) {
	atomic.AddInt64(&s.localCount, 1)
	atomic.AddInt64(&s.localTokens, tokens)
	s.analytics.recordLocal(model, tokens, genDurationMs)
	s.IncrSpill(keyName, "local")
	// Persist hourly bucket and model stat for this request, async (see
	// .local/audit-fixes-2026-08-03.md #1 - these were synchronous SQLite
	// writes on every single inference request before).
	now := time.Now().UTC().Truncate(time.Hour)
	saved := s.refCostPer1K * float64(tokens) / 1000.0
	s.enqueueStats(statsJob{
		bucket: store.HourlyBucket{
			Hour:          now,
			LocalRequests: 1,
			Tokens:        tokens,
			CostUSD:       0,
			GenDurationMs: genDurationMs,
		},
		stat: store.ModelStat{
			Model:    model,
			Requests: 1,
			Tokens:   tokens,
			CostUSD:  saved,
		},
	})
}

// enqueueStats sends a statsJob to the async writer without blocking the
// caller. If the queue is completely backed up the job is dropped and
// logged, same trade-off LogRequest already makes for logChan.
func (s *Server) enqueueStats(job statsJob) {
	select {
	case s.statsChan <- job:
	default:
		log.Printf("async logger: stats queue full, dropped hourly/model-stat update for %s", job.stat.Model)
	}
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
func (s *Server) TrackCloudCostModel(keyName, provider, model string, costPer1K float64, tokens int64) {
	atomic.AddInt64(&s.cloudCount, 1)
	atomic.AddInt64(&s.cloudTokens, tokens)
	cost := costPer1K * float64(tokens) / 1000.0
	s.mu.Lock()
	s.cloudSpentUSD += cost
	s.mu.Unlock()
	s.analytics.recordCloud(model, costPer1K, tokens)
	s.IncrSpill(keyName, provider)
	// Persist hourly bucket and model stat for this request. Kept SYNCHRONOUS
	// (unlike TrackLocalRequestModel above) because cloudSpendSince() reads
	// this exact hourly_buckets row to enforce the daily/monthly cloud spend
	// cap in CloudBudgetExceeded - making this write async would let a burst
	// of concurrent cloud requests blow through a configured budget cap
	// before the async writer catches up. CostUSD is the only field here
	// that gates a real-time decision; everything else this file made async
	// (audit log, local-request stats) has no such immediate-read dependency.
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
		// Digest is this node's runtime-reported content digest for the model,
		// when known (currently only Ollama). Empty/omitted otherwise (R1).
		Digest    string `json:"digest,omitempty"`
		Warm      bool   `json:"warm"`
		VRAMBytes int64  `json:"vram_bytes,omitempty"`
		Runtime   string `json:"runtime,omitempty"`
	}
	type modelEntry struct {
		Name     string `json:"name"`
		SizeVRAM int64  `json:"size_vram"`
		// SizeDisk is the model's on-disk size in bytes, as reported by
		// /api/tags (Ollama) or the Marbor Agent's models.list capability
		// (vLLM/TGI/llama.cpp/MLX). Zero/omitted when neither source has
		// reported it yet for this model (R1: never estimated).
		SizeDisk   int64      `json:"size_disk,omitempty"`
		Nodes      []nodeInfo `json:"nodes"`
		WarmCount  int        `json:"warm_count"`
		TotalNodes int        `json:"total_nodes"`
		// Family is Ollama's own architecture classification (e.g. "llama",
		// "bert") when known - omitted (R1) for models only ever seen via
		// /api/ps (no family field there) or via a non-Ollama agent's
		// HF-cache scan (no family metadata available - Architecture Law 5's
		// stated deferral, not a silent gap).
		Family string `json:"family,omitempty"`
		// DigestMismatch is true when 2+ nodes report different non-empty
		// digests for this model name - e.g. the same tag re-pulled with
		// different content mid-rollout. False when fewer than 2 nodes have
		// reported a digest at all (never a false positive from partial data).
		DigestMismatch bool `json:"digest_mismatch,omitempty"`
		// TotalVRAMBytes is the sum of SizeVRAM across all warm copies of this
		// model - live, not estimated, 0 when no warm copy reports a size (R1).
		TotalVRAMBytes int64 `json:"total_vram_bytes,omitempty"`
		// DriftDetails is a short inline diff of the distinct non-empty digests
		// for this model, e.g. "a1b2c3 vs c3d4e5" (truncated 6-char hex, "sha256:"
		// prefix stripped). Empty when fewer than 2 distinct digests (R1 - never
		// synthesized).
		DriftDetails string `json:"drift_details,omitempty"`
	}

	nodes := s.router.Nodes()
	modelMap := make(map[string]*modelEntry)

	type nodeSnapshot struct {
		url     string
		name    string
		healthy bool
		runtime string
		warmSet map[string]bool
	}
	snapshots := make([]nodeSnapshot, len(nodes))

	for i, n := range nodes {
		n.RLock()
		nodeURL := n.URL
		nodeName := n.Name
		nodeHealthy := n.Healthy
		nodeRuntime := n.Runtime
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
			if m.SizeVRAM > 0 {
				modelMap[m.Name].TotalVRAMBytes += m.SizeVRAM
			}
			modelMap[m.Name].Nodes = append(modelMap[m.Name].Nodes, nodeInfo{
				Name:      nodeName,
				Healthy:   nodeHealthy,
				Digest:    m.Digest,
				Warm:      true,
				VRAMBytes: m.SizeVRAM,
				Runtime:   nodeRuntime,
			})
			if nodeHealthy {
				modelMap[m.Name].WarmCount++
			}
		}
		n.RUnlock()
		snapshots[i] = nodeSnapshot{url: nodeURL, name: nodeName, healthy: nodeHealthy, runtime: nodeRuntime, warmSet: warmSet}
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
			if modelMap[tm.Name] == nil {
				modelMap[tm.Name] = &modelEntry{Name: tm.Name}
			}
			// /api/ps (the warm-models loop above) has no family field, so a
			// model already added while warm still gets enriched here once
			// /api/tags is fetched, rather than only new cold entries.
			if modelMap[tm.Name].Family == "" {
				modelMap[tm.Name].Family = tm.Details.Family
			}
			// Same enrichment for on-disk size - /api/ps has no disk-size
			// field either, only /api/tags reports it.
			if modelMap[tm.Name].SizeDisk == 0 {
				modelMap[tm.Name].SizeDisk = tm.Size
			}
			if res.snap.warmSet[tm.Name] {
				continue // node/warm-count already recorded above
			}
			modelMap[tm.Name].Nodes = append(modelMap[tm.Name].Nodes, nodeInfo{
				Name:    res.snap.name,
				Healthy: res.snap.healthy,
				Warm:    false,
				Runtime: res.snap.runtime,
			})
			// WarmCount stays 0: model is available but not in VRAM
		}
	}

	// Also include idle (downloaded, not loaded) models on nodes whose Node
	// Agent exposes the models.list capability - covers vLLM/TGI/llama.cpp/MLX
	// nodes, which have no /api/tags equivalent (Architecture Law 5). Reuses
	// handleNodeModels' own agent-dispatch path (listModelsViaAgent). Kept
	// sequential (no goroutines) to stay within this handler's existing
	// per-request node-loop shape rather than introduce a second concurrency
	// model alongside the FetchModelTags fan-out above.
	for _, snap := range snapshots {
		if !snap.healthy || snap.url == "" {
			continue
		}
		agentCfg, agentOK := s.router.MarborAgentSetting(snap.name)
		if !agentOK || !agentCfg.Enabled || !nodeHasAgentCapability(nodes, snap.name, "models.list") {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), nodeModelsListTimeout)
		models, err := s.listModelsViaAgent(ctx, snap.url, agentCfg)
		cancel()
		if err != nil {
			continue
		}
		for _, am := range models {
			entry := modelMap[am.Name]
			if entry == nil {
				entry = &modelEntry{Name: am.Name}
				modelMap[am.Name] = entry
			}
			if entry.Family == "" {
				entry.Family = am.Family
			}
			if entry.SizeDisk == 0 {
				entry.SizeDisk = am.SizeBytes
			}
			if snap.warmSet[am.Name] {
				continue // already added as a warm model above
			}
			alreadyOnNode := false
			for _, ni := range entry.Nodes {
				if ni.Name == snap.name {
					alreadyOnNode = true
					break
				}
			}
			if alreadyOnNode {
				continue // already added via FetchModelTags for this node
			}
			entry.Nodes = append(entry.Nodes, nodeInfo{
				Name:    snap.name,
				Healthy: snap.healthy,
				Warm:    false,
				Runtime: snap.runtime,
			})
			// WarmCount stays 0: model is downloaded but not in VRAM
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
		// A model "disagrees" across the fleet when 2+ nodes report a non-empty
		// digest for it and those digests aren't all the same - e.g. the same
		// tag re-pulled with different content on some nodes but not others.
		seenDigests := make(map[string]bool)
		for _, ni := range v.Nodes {
			if ni.Digest != "" {
				seenDigests[ni.Digest] = true
			}
		}
		v.DigestMismatch = len(seenDigests) > 1
		if len(seenDigests) > 1 {
			digests := make([]string, 0, len(seenDigests))
			for d := range seenDigests {
				digests = append(digests, d)
			}
			sort.Strings(digests)
			shorts := make([]string, 0, len(digests))
			for _, d := range digests {
				s := d
				if strings.HasPrefix(s, "sha256:") {
					s = strings.TrimPrefix(s, "sha256:")
				}
				if len(s) > 6 {
					s = s[:6]
				}
				if s == "" {
					s = "-"
				}
				shorts = append(shorts, s)
			}
			v.DriftDetails = strings.Join(shorts, " vs ")
		}
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

// nodePullTimeout bounds how long marbor waits for a model pull to
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
// enough that the map doesn't grow unbounded across a long-running marbor
// process with many pulls over time.
const pullJobMaxAge = 10 * time.Minute

// pullVerifyTimeout bounds the single load-verification chat-completion
// request (opt-in "verify it loads" pulls only) - generous like
// benchmarkSampleTimeout, since a large model's cold load through the proxy
// can genuinely take a while. Bounded by the outer pullCtx (nodePullTimeout)
// regardless.
var pullVerifyTimeout = 5 * time.Minute

// pullVerifyKeyTTL is defense-in-depth only, mirroring benchmarkKeyTTL's
// reasoning: the ephemeral key is always explicitly revoked/deleted when
// verifyModelLoads returns, on every exit path: this TTL just bounds how
// long an orphaned key could authenticate if that cleanup were ever skipped
// by a crash mid-probe.
const pullVerifyKeyTTL = 15 * time.Minute

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
	Status         string    `json:"status"` // "downloading" | "verifying" | "success" | "failed" | "load_failed" | "cancelled"
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	BytesTotal     int64     `json:"bytes_total,omitempty"`
	BytesCompleted int64     `json:"bytes_completed,omitempty"`
	Error          string    `json:"error,omitempty"`
	// cancel aborts the in-flight pull's context - unexported, so it never
	// appears in the JSON progress payload the UI reads. Canceling the
	// marbor's own outbound HTTP request (direct path: the streaming pull;
	// agent path: the call to the agent) is real cancellation, not cosmetic:
	// Go's http.Server cancels the handler's request context when the
	// client connection drops, and the agent's action handler ties
	// exec.CommandContext to that same context - so a cancel from the admin
	// UI actually kills the download subprocess on the node, not just the
	// marbor's view of it.
	cancel context.CancelFunc
	// verifyLoad records whether the client opted into a post-download load
	// verification (a real chat-completion probe, not a guess - see
	// verifyModelLoads) before reporting "success". Set once at job creation,
	// never appears in the JSON payload - the UI already knows what it asked
	// for; this just controls completePull's behavior.
	verifyLoad bool
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
	VerifyLoad     bool      `json:"verify_load"`
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
		VerifyLoad: j.verifyLoad,
	}
}

func (j *pullJob) setProgress(total, completed int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.BytesTotal, j.BytesCompleted = total, completed
}

// pullJobActive reports whether status is a non-terminal, in-progress state:
// "downloading" (fetching bytes) or "verifying" (opt-in load-verification
// probe, only reached after a successful download). Every other status
// (success/failed/load_failed/cancelled) is terminal.
func pullJobActive(status string) bool {
	return status == "downloading" || status == "verifying"
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
	if !pullJobActive(j.Status) {
		return
	}
	j.Status = status
	j.Error = errMsg
	j.FinishedAt = time.Now()
}

// setVerifying transitions j from "downloading" to "verifying" - the
// download succeeded and completePull is about to run a load-verification
// probe before reporting a final outcome. Returns false if the job is
// already terminal (e.g. cancelled mid-download), telling the caller not to
// start the probe at all.
func (j *pullJob) setVerifying() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status != "downloading" {
		return false
	}
	j.Status = "verifying"
	return true
}

// requestCancel marks j cancelled (if still in progress) and invokes its
// context cancel func. Returns false if the job was already terminal - the
// caller uses this to tell "cancelled" from "too late, already finished".
// Cancelling during "verifying" aborts the in-flight load-verification probe
// the same way it aborts an in-flight download - both share pullCtx.
func (j *pullJob) requestCancel() bool {
	j.mu.Lock()
	if !pullJobActive(j.Status) {
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
		// pullJobActive, not "!= downloading": a job mid-verification has no
		// FinishedAt yet either, and would otherwise read as ancient
		// (time.Since(zero value)) and get swept mid-probe.
		if !pullJobActive(snap.Status) && time.Since(snap.FinishedAt) > pullJobMaxAge {
			delete(s.pullJobs, key)
		}
	}
}

// handleListActivePulls returns every currently in-progress pull job (still
// downloading or running its post-download load-verification probe) across
// all nodes. It exists so the UI can restore its progress widget after a
// browser refresh wipes the client-side tracking state (pullProgress.ts's
// job map is module-level, in-memory JS - gone on reload) - the client has
// no other way to learn which (node, model) keys have a job in flight to
// resubscribe to. Only in-progress jobs are returned: a finished job's
// terminal state is only useful to a client that was already watching it
// live, not one restoring cold after a reload.
func (s *Server) handleListActivePulls(w http.ResponseWriter, r *http.Request) {
	s.pullsMu.Lock()
	snaps := make([]pullJobSnapshot, 0, len(s.pullJobs))
	for _, j := range s.pullJobs {
		snap := j.snapshot()
		if pullJobActive(snap.Status) {
			snaps = append(snaps, snap)
		}
	}
	s.pullsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snaps)
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
		// VerifyLoad opts into a post-download load-verification probe (see
		// completePull/verifyModelLoads) before the job reports "success" -
		// catches a model whose GGUF architecture downloads fine but can't
		// actually be loaded by this node's installed runtime version, which
		// otherwise surfaces later as a cryptic failure the first time
		// something tries to actually use the model.
		VerifyLoad bool `json:"verify_load"`
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

	// Fetched once and reused below (nodeIsHealthy, the disk gate,
	// nodeHasAgentCapability) rather than re-snapshotting the fleet's node
	// list under its own RLock on every check in this handler.
	nodes := s.router.Nodes()

	// A down node's URL may still be answering (e.g. some other service
	// listening on that port), producing a confusing upstream error that
	// looks model-specific when the real problem is just node reachability.
	// Fail fast with an honest reason instead.
	if !nodeIsHealthy(nodes, nodeName) {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("node %q is currently unreachable (down) - check its URL/connectivity before pulling", nodeName))
		return
	}

	// Reject a pull whose tag format this node's runtime can never load,
	// before any bytes move - see catalog.go's classifyPullTagFormat/
	// pullFormatIncompatible doc comments for the confident-only compatibility
	// matrix across all 5 runtimes (P70). Without this, an "ollama-library"
	// or "gguf-hf" tag pulled onto an incompatible runtime either fails deep
	// into a multi-GB huggingface-cli download with a cryptic subprocess
	// error, or - for a GGUF repo id stripped down to "org/repo" - downloads
	// completely successfully and only turns out to be unloadable the first
	// time something tries to use it.
	if nodeRuntime, ok := nodeRuntimeByName(nodes, nodeName); ok {
		format := classifyPullTagFormat(body.Model)
		if pullFormatIncompatible(format, nodeRuntime) {
			writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
				"%q looks like %s, but node %q runs %q, which cannot load that format",
				body.Model, pullFormatDescription(format), nodeName, nodeRuntime))
			return
		}
	}

	// Hard-block a pull that free disk cannot possibly satisfy - unlike VRAM
	// fit (soft confirm, see P47), a disk overrun is a guaranteed failure
	// (partial download, or worst case fills the node's disk and disrupts
	// the OS/other running models), so there is no confirm-anyway override.
	// Disk state is fetched once and reused by whichever classification
	// below applies (P73: previously this fetch, and the entire gate, lived
	// only inside the known-size branch below, so any tag outside the
	// curated catalog skipped the check entirely rather than falling back to
	// a policy floor - see EXECUTION-QUEUE.md P73).
	diskFreeGB, diskTotalGB, agentPresent := nodeDiskState(nodes, nodeName)
	// For a Docker-controlled node, the host-level reading above can be
	// wrong: Ollama's actual model storage may live on a separate,
	// differently-sized container volume/mount, while this agent-reported
	// figure only ever reflects the *host's* root filesystem. When an
	// agent capable of reporting the container's own real disk stats is
	// available, prefer that fresh, on-demand reading over the periodic
	// (and for this case, potentially misleading) telemetry snapshot for
	// this one safety-critical decision - this is exactly the gap that
	// let a disk-full pull fail deep into a multi-GB transfer instead of
	// being caught before it started.
	if ctrl, configured := s.router.NodeControlSetting(nodeName); configured && ctrl.Driver == "docker" {
		if agentCfg, agentOK := s.router.MarborAgentSetting(nodeName); agentOK && agentCfg.Enabled && nodeHasAgentCapability(nodes, nodeName, "runtime.disk") {
			if freeB, totalB, err := s.containerDiskStatsViaAgent(r.Context(), nodeURL, agentCfg, ctrl); err == nil && totalB > 0 {
				diskFreeGB = float64(freeB) / (1024 * 1024 * 1024)
				diskTotalGB = float64(totalB) / (1024 * 1024 * 1024)
				agentPresent = true
			}
			// On error, silently keep the host-level reading already
			// computed above - this extra safety check failing to run
			// must never itself become a reason to block a pull outright.
		}
	}
	if sizeMB, known := findCatalogVariantSizeMB(body.Model); known {
		if classifyDiskFit(sizeMB, diskFreeGB, diskTotalGB, agentPresent) == "insufficient" {
			writeJSONError(w, http.StatusInsufficientStorage, fmt.Sprintf(
				"insufficient disk space on node %q: %q needs ~%.1f GB, only %.1f GB free",
				nodeName, body.Model, float64(sizeMB)/1024, diskFreeGB))
			return
		}
	} else if classifyUnknownSizeDiskFit(diskFreeGB, diskTotalGB, agentPresent) == "insufficient" {
		// body.Model isn't in the curated catalog, so its real download size
		// is unknown (P73) - classifyDiskFit's size-vs-free-space test cannot
		// run. Refuse rather than let an unsized download proceed against a
		// node that is already low on headroom; see classifyUnknownSizeDiskFit's
		// doc comment for why this is a policy floor and not a guessed size.
		writeJSONError(w, http.StatusInsufficientStorage, fmt.Sprintf(
			"refusing to pull %q on node %q: its download size is unknown (not in the curated catalog) and only %.1f GB of %.1f GB disk is free, below the safety margin required for an unsized pull - free up disk space or use a curated catalog model with a known size",
			body.Model, nodeName, diskFreeGB, diskTotalGB))
		return
	}

	s.sweepOldPullJobs()

	// Dedup concurrent pulls of the same model on the same node. State is
	// ephemeral and in-memory only - it is never persisted and never wired
	// into placement/warm-residency scoring, it just prevents two admin
	// clicks from racing the same multi-GB download.
	pullKey := nodeName + "|" + body.Model
	s.pullsMu.Lock()
	if existing, ok := s.pullJobs[pullKey]; ok && pullJobActive(existing.snapshot().Status) {
		s.pullsMu.Unlock()
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("pull already in progress for %q on node %q", body.Model, nodeName))
		return
	}

	agentCfg, agentOK := s.router.MarborAgentSetting(nodeName)
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
	job := &pullJob{Node: nodeName, Model: body.Model, Method: method, Status: "downloading", StartedAt: time.Now(), cancel: cancel, verifyLoad: body.VerifyLoad}
	s.pullJobs[pullKey] = job
	s.pullsMu.Unlock()

	// logPullOutcome records the pull as a system-audit event regardless of
	// which terminal state it lands in - "load_failed" (downloaded fine, but
	// the model can't actually be loaded here) is exactly as worth an
	// operator's attention as "failed" (the download itself never
	// succeeded), just for a different reason.
	logPullOutcome := func() {
		switch job.snapshot().Status {
		case "success":
			suffix := ""
			if useAgent {
				suffix = " (via agent)"
			}
			s.logSystemChange(r, "pull_model", nodeName, fmt.Sprintf("Model: %s%s", body.Model, suffix))
		case "load_failed":
			s.logSystemChange(r, "pull_model_load_failed", nodeName, fmt.Sprintf("Model: %s - %s", body.Model, job.snapshot().Error))
		}
	}

	if useAgent {
		go func() {
			defer cancel()
			ctrl, _ := s.router.NodeControlSetting(nodeName)
			err := s.pullModelViaAgent(pullCtx, nodeURL, agentCfg, body.Model, ctrl)
			if err != nil {
				job.finish("failed", err.Error())
				return
			}
			s.completePull(pullCtx, job)
			logPullOutcome()
		}()
	} else {
		go func() {
			defer cancel()
			s.runDirectPull(pullCtx, job, nodeURL, body.Model)
			logPullOutcome()
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
			// no field for a Hugging Face token - only a Marbor Agent can
			// deliver one, via HF_TOKEN in the pull subprocess's own
			// environment (actions.go runDownload). A 401 here almost always
			// means a gated/token-required HF model on a node without an
			// agent, not a marbor misconfiguration.
			errMsg += " (this node has no marbor agent capable of pull_model - token-gated Hugging Face pulls require one; install/enable the marbor agent on this node or use a non-gated model)"
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
	s.completePull(ctx, job)
}

// completePull finishes a job whose download already succeeded: runs the
// opt-in load-verification probe first when the client requested one, then
// finishes with the real outcome - "success" (verified, or unverified if the
// client didn't opt in) or "load_failed" (downloaded fine, but didn't
// survive an actual load attempt - most often an unsupported GGUF
// architecture on this node's installed runtime version, the exact class of
// failure a bare pull success can't catch). ctx is pullCtx, so cancelling
// the pull (or its 2h timeout firing) also aborts an in-progress probe.
func (s *Server) completePull(ctx context.Context, job *pullJob) {
	if !job.verifyLoad {
		job.finish("success", "")
		return
	}
	if !job.setVerifying() {
		return // already terminal (e.g. cancelled mid-download)
	}
	if err := s.verifyModelLoads(ctx, job.Node, job.Model); err != nil {
		job.finish("load_failed", err.Error())
		return
	}
	job.finish("success", "")
	// The probe's job is to prove loadability, not to warm the model up -
	// leaving it resident would be a surprise side effect of a compatibility
	// check, and could evict whatever the operator actually wanted warm.
	// Best-effort: a failure here doesn't change the pull's own outcome.
	s.bestEffortUnloadAfterVerify(job.Node, job.Model)
}

// verifyModelLoads sends one minimal chat-completion request through the
// marbor's own proxy (bench.MeasureChatTTFT - the same probe the Hardware
// Benchmark page uses) to confirm the model actually loads and answers, not
// just that its blob downloaded successfully. Reuses handleRunBenchmark's
// exact ephemeral-scoped-key pattern so this probe exercises the real
// client-auth path too, rather than needing an admin-bypass.
func (s *Server) verifyModelLoads(ctx context.Context, nodeName, model string) error {
	if s.cfg.Proxy.Port <= 0 {
		return fmt.Errorf("proxy port is not configured - cannot verify model load")
	}
	keyName := fmt.Sprintf("pull-verify-%s-%s-%d", nodeName, model, time.Now().UnixNano())
	keyValue, err := generateAPIKey(keyName)
	if err != nil {
		return fmt.Errorf("generate verification key: %w", err)
	}
	k := config.KeyConfig{
		Name:      keyName,
		Key:       keyValue,
		Models:    []string{model},
		ExpiresAt: time.Now().Add(pullVerifyKeyTTL).Format(time.RFC3339),
	}
	if s.auth != nil {
		s.auth.AddKey(k)
	}
	_ = s.st.UpsertKey(store.KeyRecord{Name: k.Name, Key: k.Key, Models: k.Models, Revoked: false, ExpiresAt: k.ExpiresAt})
	defer func() {
		if s.auth != nil {
			s.auth.RevokeKey(k.Name)
		}
		_ = s.st.DeleteKey(k.Name)
	}()

	target := fmt.Sprintf("http://localhost:%d", s.cfg.Proxy.Port)
	client := &http.Client{Timeout: pullVerifyTimeout}
	_, err = bench.MeasureChatTTFT(ctx, client, target, model, k.Key)
	return err
}

// bestEffortUnloadAfterVerify evicts a just-verified model right after a
// successful load-verification probe (see completePull). Mirrors
// handleUnloadModel's own agent-vs-direct dispatch decision
// (ShouldUseAgentForUnload) rather than duplicating a second copy of that
// logic. A pinned model is left alone (pinning means "never evict without an
// explicit unpin first," which a background verification step must respect
// same as a manual unload would) and isn't logged as a failure.
func (s *Server) bestEffortUnloadAfterVerify(nodeName, model string) {
	ctx, cancel := context.WithTimeout(context.Background(), nodeUnloadModelTimeout)
	defer cancel()

	agentCfg, useAgent := s.router.ShouldUseAgentForUnload(nodeName)
	var err error
	if useAgent {
		nodeURL := s.router.NodeURLs()[nodeName]
		ctrl, _ := s.router.NodeControlSetting(nodeName)
		err = s.unloadModelViaAgent(ctx, nodeURL, agentCfg, model, ctrl)
	} else {
		_, err = s.router.UnloadModel(ctx, nodeName, model)
	}
	if err != nil && !errors.Is(err, router.ErrModelPinned) {
		log.Printf("bestEffortUnloadAfterVerify: failed to unload %q on node %q after load-verification: %v", model, nodeName, err)
	}
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

		if !pullJobActive(snap.Status) {
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
// Cancellation is real, not cosmetic: it cancels the marbor's own outbound
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
//
// This reflects RUNTIME reachability (the router's poll of the node's own
// inference-runtime URL, health.go's pollNode/probe.Probe) - correct for
// gating anything that proxies inference traffic or reads runtime-served
// data (pull/models/delete-model), but wrong for gating a call that talks
// to the Marbor Agent instead (see marborAgentIsPresent below): an operator
// intentionally stopping the runtime via Runtime Control makes this false
// by design, and using it there would make "Start" unreachable right
// after "Stop" ever succeeded once.
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

// marborAgentIsPresent reports whether the node named name's Marbor Agent
// answered the marbor's last poll (router.NodeState.AgentPresent, set by
// agent_poll.go - independent of runtime reachability, per that file's own
// "the agent is a fully independent HTTP endpoint from /api/ps" reasoning).
// Use this, not nodeIsHealthy, to gate any admin handler that dispatches to
// the agent itself (runtime start/stop/restart, runtime logs) rather than
// to the runtime it manages - the whole point of these calls is to work
// while the runtime is stopped. Returns false if the node isn't found,
// same as nodeIsHealthy.
func marborAgentIsPresent(nodes []*router.NodeState, name string) bool {
	for _, n := range nodes {
		if n.Name != name {
			continue
		}
		n.RLock()
		present := n.AgentPresent
		n.RUnlock()
		return present
	}
	return false
}

// nodeModelsListTimeout bounds how long the admin API waits for a node
// agent's GET /v1/models response. Short and synchronous, unlike pulls
// (async, nodePullTimeout) - a list is a read, never a multi-GB transfer, so
// there is no legitimate reason for the request to run long.
var nodeModelsListTimeout = 15 * time.Second

// nodeModelEntry is the admin API's JSON shape for one locally-available
// model on a node - camelCase per this API's own convention, translated
// from the agent's snake_case wire shape by listModelsViaAgent below.
type nodeModelEntry struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Source    string `json:"source"`
	// Family is Ollama's own architecture classification (e.g. "llama",
	// "bert") when the agent's source could report it (ollama-tags only;
	// hf-cache scans have no such metadata) - see marboragent.modelEntry.
	Family string `json:"family,omitempty"`
}

// handleNodeModels lists models already downloaded (not just currently
// loaded - see the node's own "Loaded Models" field for that) on a specific
// node, via its Marbor Agent's GET /v1/models (capability "models.list"). No
// direct-HTTP fallback exists for this today (unlike handleNodePull) - a
// node without an agent, or an agent build predating this capability,
// returns a clear 501 rather than a fabricated empty-but-successful list
// (R1).
func (s *Server) handleNodeModels(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("name")

	urls := s.router.NodeURLs()
	nodeURL, ok := urls[nodeName]
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
		return
	}

	// Same fail-fast reasoning as handleNodePull: a down node's URL may
	// still answer with something (another service on that port), producing
	// a confusing "agent list models failed: ..." error that looks
	// capability-specific when the real problem is just reachability.
	if !nodeIsHealthy(s.router.Nodes(), nodeName) {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("node %q is currently unreachable (down) - check its URL/connectivity before listing models", nodeName))
		return
	}

	agentCfg, agentOK := s.router.MarborAgentSetting(nodeName)
	if !agentOK || !agentCfg.Enabled || !nodeHasAgentCapability(s.router.Nodes(), nodeName, "models.list") {
		writeJSONError(w, http.StatusNotImplemented, fmt.Sprintf("node %q has no agent capability for listing local models", nodeName))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), nodeModelsListTimeout)
	defer cancel()
	models, err := s.listModelsViaAgent(ctx, nodeURL, agentCfg)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"models": models})
}

// listModelsViaAgent queries nodeURL's Marbor Agent (GET /v1/models,
// capability "models.list") and translates its snake_case wire response
// into this API's camelCase nodeModelEntry shape.
func (s *Server) listModelsViaAgent(ctx context.Context, nodeURL string, agentCfg router.MarborAgentConfig) ([]nodeModelEntry, error) {
	actionURL, err := buildAgentURL(nodeURL, agentCfg.Port, agentCfg.Scheme, "/v1/models")
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, actionURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+agentCfg.Token)

	resp, err := s.router.HTTPClientForNode(nodeModelsListTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent list models failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var out struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("agent returned %d", resp.StatusCode)
		}
		return nil, errors.New(msg)
	}

	var out struct {
		Models []struct {
			Name      string `json:"name"`
			SizeBytes int64  `json:"size_bytes"`
			Source    string `json:"source"`
			Family    string `json:"family"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agent list models: could not decode response (status %d)", resp.StatusCode)
	}
	models := make([]nodeModelEntry, 0, len(out.Models))
	for _, m := range out.Models {
		models = append(models, nodeModelEntry{Name: m.Name, SizeBytes: m.SizeBytes, Source: m.Source, Family: m.Family})
	}
	return models, nil
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

// pullModelViaAgent dispatches a model pull to nodeURL's Marbor Agent
// (POST /v1/models, capability "models.pull") instead of the node's own
// runtime HTTP API, forwarding the marbor's configured Hugging Face token
// per-request - never stored on the agent side, only set in the pull
// subprocess's own environment for its lifetime (see
// .local/specs/node-agent.md section 16).
func (s *Server) pullModelViaAgent(ctx context.Context, nodeURL string, agentCfg router.MarborAgentConfig, model string, ctrl router.ControlConfig) error {
	actionURL, err := buildAgentURL(nodeURL, agentCfg.Port, agentCfg.Scheme, "/v1/models")
	if err != nil {
		return err
	}

	// Driver/Identifier are only meaningful when ctrl.Driver == "docker": the
	// agent's own download commands (ollama/huggingface-cli/etc.) run as a
	// host subprocess by default, which fails when the runtime is actually
	// inside a container - see actions.go's runDownload doc comment. Passed
	// through unconditionally (empty strings when unconfigured) exactly like
	// runtimeActionViaAgent does, so a node with no control driver configured
	// (the common systemd/native-process case) sees no behavior change.
	reqBody, err := json.Marshal(map[string]string{
		"model":      model,
		"hf_token":   s.cfg.HuggingFace.Token,
		"driver":     ctrl.Driver,
		"identifier": ctrl.Identifier,
	})
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

	resp, err := s.router.HTTPClientForNode(nodePullTimeout).Do(req)
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

// containerDiskStatsViaAgent asks nodeURL's Marbor Agent for the real disk
// stats of the container ctrl identifies (POST /v1/runtime/disk, capability
// "runtime.disk") - see internal/marboragent's handleRuntimeDisk for why this
// can differ from the host-level DiskFreeGB/DiskTotalGB the periodic
// telemetry poll already reports for a Docker-controlled node.
func (s *Server) containerDiskStatsViaAgent(ctx context.Context, nodeURL string, agentCfg router.MarborAgentConfig, ctrl router.ControlConfig) (freeBytes, totalBytes int64, err error) {
	actionURL, err := buildAgentURL(nodeURL, agentCfg.Port, agentCfg.Scheme, "/v1/runtime/disk")
	if err != nil {
		return 0, 0, err
	}
	reqBody, err := json.Marshal(map[string]string{"driver": ctrl.Driver, "identifier": ctrl.Identifier})
	if err != nil {
		return 0, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL, bytes.NewReader(reqBody))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentCfg.Token)

	resp, err := s.router.HTTPClientForNode(10 * time.Second).Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("agent disk stats failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		FreeBytes  int64  `json:"free_bytes"`
		TotalBytes int64  `json:"total_bytes"`
		Error      string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0, fmt.Errorf("agent disk stats: could not decode response (status %d)", resp.StatusCode)
	}
	if out.Error != "" {
		return 0, 0, errors.New(out.Error)
	}
	return out.FreeBytes, out.TotalBytes, nil
}

// buildAgentURL derives an agent URL from the node's own URL (same host, via
// url.Parse per R5 - never arithmetic port derivation), the configured agent
// port, a literal path, and the agent's OWN scheme (independent of nodeURL's
// scheme - see store.MarborAgentRecord.Scheme's doc comment; before this
// parameter existed, every agent action derived its scheme from the node's
// runtime URL instead, so enabling agent HTTPS silently switched the
// runtime endpoint to https:// too). Mirrors agent_poll.go's buildAgentURL
// in internal/router (kept as a separate small function since admin and
// router are different packages).
func buildAgentURL(nodeURL string, port int, scheme string, path string) (string, error) {
	u, err := url.Parse(nodeURL)
	if err != nil {
		return "", fmt.Errorf("parse node URL: %w", err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("node URL %q has no host", nodeURL)
	}
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, u.Hostname(), port, path), nil
}

// nodeDeleteModelTimeout bounds how long the admin API waits for a node
// agent's DELETE /v1/models/{name} response. Short like
// nodeModelsListTimeout, not generous like nodePullTimeout - a delete is a
// local filesystem removal, never a multi-GB transfer, so there is no
// legitimate reason for it to run long.
var nodeDeleteModelTimeout = 60 * time.Second

// handleNodeDeleteModel removes a locally-downloaded model from a specific
// node, via its Marbor Agent's DELETE /v1/models/{name} (capability
// "models.delete"). No direct-HTTP fallback exists for this today (same
// reasoning as handleNodeModels) - a node without an agent, or an agent
// build predating this capability, returns a clear 501 rather than
// silently reporting success for a delete that never happened (R1 extended
// to actions).
func (s *Server) handleNodeDeleteModel(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("name")
	model := r.PathValue("model")
	if model == "" {
		writeJSONError(w, http.StatusBadRequest, "missing model name in path")
		return
	}

	urls := s.router.NodeURLs()
	nodeURL, ok := urls[nodeName]
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
		return
	}

	// Same fail-fast reasoning as handleNodePull/handleNodeModels: a down
	// node's URL may still answer with something (another service on that
	// port), producing a confusing "agent delete model failed: ..." error
	// that looks capability-specific when the real problem is just
	// reachability.
	if !nodeIsHealthy(s.router.Nodes(), nodeName) {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("node %q is currently unreachable (down) - check its URL/connectivity before deleting a model", nodeName))
		return
	}

	agentCfg, agentOK := s.router.MarborAgentSetting(nodeName)
	if !agentOK || !agentCfg.Enabled || !nodeHasAgentCapability(s.router.Nodes(), nodeName, "models.delete") {
		writeJSONError(w, http.StatusNotImplemented, fmt.Sprintf("node %q has no agent capability for deleting local models", nodeName))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), nodeDeleteModelTimeout)
	defer cancel()
	ctrl, _ := s.router.NodeControlSetting(nodeName)
	if err := s.deleteModelViaAgent(ctx, nodeURL, agentCfg, model, ctrl); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.logSystemChange(r, "delete_model", nodeName, fmt.Sprintf("Model: %s", model))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// deleteModelViaAgent dispatches a model delete to nodeURL's Marbor Agent
// (DELETE /v1/models/{name}, capability "models.delete"). ctrl carries the
// node's configured driver/identifier (same router.ControlConfig cache
// runtimeActionViaAgent reads) so the agent knows to route the delete
// through `docker exec` when the runtime is Docker-controlled - see
// pullModelViaAgent's identical reasoning.
func (s *Server) deleteModelViaAgent(ctx context.Context, nodeURL string, agentCfg router.MarborAgentConfig, model string, ctrl router.ControlConfig) error {
	actionURL, err := buildAgentURL(nodeURL, agentCfg.Port, agentCfg.Scheme, "/v1/models/"+escapeModelPathSegments(model))
	if err != nil {
		return err
	}

	reqBody, err := json.Marshal(map[string]string{"driver": ctrl.Driver, "identifier": ctrl.Identifier})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, actionURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentCfg.Token)

	resp, err := s.router.HTTPClientForNode(nodeDeleteModelTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("agent delete model failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("agent delete model: could not decode response (status %d)", resp.StatusCode)
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

// escapeModelPathSegments percent-escapes each "/"-delimited segment of a
// model name independently, then rejoins with literal "/" - preserving the
// path-segment split the agent's "{name...}" wildcard route expects while
// still escaping characters ('#', '?', space, ...) that would otherwise be
// reinterpreted as a fragment/query boundary by url.Parse/http.NewRequest,
// silently truncating the path to a different (shorter) model name than the
// caller intended.
func escapeModelPathSegments(model string) string {
	parts := strings.Split(model, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// nodeHealthCheckTimeout bounds how long the admin API waits for an
// on-demand liveness probe. Short, like nodeModelsListTimeout - an operator
// hitting "check now" wants a fast answer, not something that can hang as
// long as a model transfer.
var nodeHealthCheckTimeout = 15 * time.Second

// nodeTLSProbeTimeout bounds the one-off TLS-handshake-only probe dial used
// by handleNodeTLSProbe. Short: this is a single local-network TCP+TLS
// handshake, not a transfer.
var nodeTLSProbeTimeout = 10 * time.Second

// handleNodeTLSProbe performs the P24 enrollment probe (spec section 2,
// step 2-3): dials the node's Marbor Agent (NOT its runtime URL - the Agent's
// own host:port, using the Agent's own configured scheme, which is
// independent of the runtime endpoint's scheme, see
// store.MarborAgentRecord.Scheme's doc comment) with a TLS-handshake-only
// connection - no bearer token sent, no certificate validated against any
// CA or existing pin - and reports the presented leaf certificate's SHA-256
// fingerprint for the operator to compare against what "agent service
// status" prints on the node itself. This never pins anything; pinning only
// happens via a subsequent PATCH /admin/nodes/{name} with tls_fingerprint
// set to the value the operator confirmed here.
func (s *Server) handleNodeTLSProbe(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	host, found := s.router.NodeHost(name)
	if !found {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	agentCfg, ok := s.router.MarborAgentSetting(name)
	if !ok || !agentCfg.Enabled {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("node %q has no marbor agent configured - enable the Agent before probing", name))
		return
	}
	if agentCfg.Scheme != "https" {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("node %q's Agent is not configured for https:// - enable Agent HTTPS before probing for a certificate fingerprint", name))
		return
	}
	port := strconv.Itoa(agentCfg.Port)

	ctx, cancel := context.WithTimeout(r.Context(), nodeTLSProbeTimeout)
	defer cancel()

	dialer := &net.Dialer{}
	rawConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("could not reach %s:%s: %v", host, port, err))
		return
	}
	defer rawConn.Close()

	// InsecureSkipVerify and no VerifyPeerCertificate deliberately: this
	// probe's entire purpose is TOFU - see whatever cert is presented,
	// unconditionally, then hand it to the operator for out-of-band
	// confirmation. It never decides trust itself.
	tlsConn := tls.Client(rawConn, &tls.Config{InsecureSkipVerify: true, ServerName: host})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("TLS handshake with %s:%s failed: %v", host, port, err))
		return
	}
	defer tlsConn.Close()

	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("node %q's Agent presented no TLS certificate", name))
		return
	}

	fingerprint := router.CertFingerprintSHA256(certs[0].Raw)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"fingerprint": fingerprint})
}

// nodeHealthCheckResult is this admin API's JSON response for an on-demand
// health check - relayed verbatim from the agent's own healthCheckResult
// shape (camelCase field names aside), never re-derived or fabricated (R1).
// LatencyMs has no omitempty - a genuinely fast (0ms) probe is a real
// measurement, not an absent one (see healthCheckResult's doc comment,
// internal/marboragent/actions.go, for the same reasoning on the agent side).
type nodeHealthCheckResult struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
}

// handleNodeHealthCheck triggers an on-demand active liveness probe for a
// node - distinct from the passive, poll-cycle-cached health already
// carried on NodeState (populated from telemetry, up to one poll interval
// stale). Prefers the node's Marbor Agent (GET /v1/runtime/health,
// capability "runtime.health_check") when available; falls back to
// router.ProbeNodeOnDemand (the same RuntimeProbe the periodic poller
// uses, read-only) for a node with no agent or an agent build predating
// this capability, so "check now" works on every node, not just
// agent-equipped ones. Both paths report a real measurement or a real
// error - never a fabricated result (R1).
func (s *Server) handleNodeHealthCheck(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("name")

	urls := s.router.NodeURLs()
	nodeURL, ok := urls[nodeName]
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
		return
	}

	// Same fail-fast reasoning as handleNodeModels/handleNodePull: a down
	// node's URL may still answer with something (another service on that
	// port), producing a confusing "agent health check failed: ..." error
	// that looks capability-specific when the real problem is just
	// reachability.
	if !nodeIsHealthy(s.router.Nodes(), nodeName) {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("node %q is currently unreachable (down) - check its URL/connectivity before running a health check", nodeName))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), nodeHealthCheckTimeout)
	defer cancel()

	agentCfg, agentOK := s.router.MarborAgentSetting(nodeName)
	if !agentOK || !agentCfg.Enabled || !nodeHasAgentCapability(s.router.Nodes(), nodeName, "runtime.health_check") {
		ok, errMsg, latencyMs, found := s.router.ProbeNodeOnDemand(ctx, nodeName)
		if !found {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(nodeHealthCheckResult{OK: ok, Error: errMsg, LatencyMs: latencyMs})
		return
	}

	result, err := s.healthCheckViaAgent(ctx, nodeURL, agentCfg)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// healthCheckViaAgent queries nodeURL's Marbor Agent (GET /v1/runtime/health,
// capability "runtime.health_check") and relays its real ok/error/latency_ms
// result. Unlike pullModelViaAgent/deleteModelViaAgent, a probe that comes
// back ok:false is not a transport error - it's the health check doing its
// job and reporting the runtime is actually down - so that path returns a
// populated nodeHealthCheckResult, not an error; err is reserved for
// genuine transport/dispatch failures (can't reach the agent itself, bad
// response shape).
func (s *Server) healthCheckViaAgent(ctx context.Context, nodeURL string, agentCfg router.MarborAgentConfig) (nodeHealthCheckResult, error) {
	actionURL, err := buildAgentURL(nodeURL, agentCfg.Port, agentCfg.Scheme, "/v1/runtime/health")
	if err != nil {
		return nodeHealthCheckResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, actionURL, nil)
	if err != nil {
		return nodeHealthCheckResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+agentCfg.Token)

	resp, err := s.router.HTTPClientForNode(nodeHealthCheckTimeout).Do(req)
	if err != nil {
		return nodeHealthCheckResult{}, fmt.Errorf("agent health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var out struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("agent returned %d", resp.StatusCode)
		}
		return nodeHealthCheckResult{}, errors.New(msg)
	}

	var out struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		LatencyMs int64  `json:"latency_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nodeHealthCheckResult{}, fmt.Errorf("agent health check: could not decode response (status %d)", resp.StatusCode)
	}
	return nodeHealthCheckResult{OK: out.OK, Error: out.Error, LatencyMs: out.LatencyMs}, nil
}

// nodeUnloadModelTimeout bounds how long the admin API waits for a node
// agent's POST /v1/models/{name} (unload) response. Short like
// nodeDeleteModelTimeout - a local runtime call, never a transfer.
var nodeUnloadModelTimeout = 30 * time.Second

// unloadModelViaAgent dispatches a model unload to nodeURL's Marbor Agent
// (POST /v1/models/{name...}, capability "models.unload") instead of the
// node's own runtime HTTP API. See actions.go's handleUnloadModel for why
// POST (not a literal "/unload" suffix) is the verb used on this path shape.
func (s *Server) unloadModelViaAgent(ctx context.Context, nodeURL string, agentCfg router.MarborAgentConfig, model string, ctrl router.ControlConfig) error {
	actionURL, err := buildAgentUnloadURL(nodeURL, agentCfg.Port, agentCfg.Scheme, model)
	if err != nil {
		return err
	}

	reqBody, err := json.Marshal(map[string]string{"driver": ctrl.Driver, "identifier": ctrl.Identifier})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentCfg.Token)

	resp, err := s.router.HTTPClientForNode(nodeUnloadModelTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("agent unload model failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("agent unload model: could not decode response (status %d)", resp.StatusCode)
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

// buildAgentUnloadURL derives the agent's POST /v1/models/{name...} URL from
// the node's own URL (same host, via url.Parse per R5 - never arithmetic
// port derivation), the configured agent port, and the agent's OWN scheme
// (independent of nodeURL's scheme - see buildAgentURL's doc comment).
// Reuses escapeModelPathSegments (same reasoning as buildAgentDeleteURL):
// model is percent-escaped per "/"-delimited segment so a name containing
// '#'/'?'/spaces can't be reinterpreted as a fragment/query boundary, while
// the segment split itself is preserved for the agent's "{name...}" wildcard.
func buildAgentUnloadURL(nodeURL string, port int, scheme string, model string) (string, error) {
	u, err := url.Parse(nodeURL)
	if err != nil {
		return "", fmt.Errorf("parse node URL: %w", err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("node URL %q has no host", nodeURL)
	}
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d/v1/models/%s", scheme, u.Hostname(), port, escapeModelPathSegments(model)), nil
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
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n > 0 && n <= 200 {
				limit = n
			} else {
				writeJSONError(w, http.StatusBadRequest, "limit must be 1..200")
				return
			}
		} else {
			writeJSONError(w, http.StatusBadRequest, "invalid limit")
			return
		}
	}
	// Enterprise filters - all optional, combined with AND.
	var fromPtr, toPtr, beforePtr *time.Time
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid from, want RFC3339")
			return
		}
		utc := t.UTC()
		fromPtr = &utc
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid to, want RFC3339")
			return
		}
		utc := t.UTC()
		toPtr = &utc
	}
	if fromPtr != nil && toPtr != nil && fromPtr.After(*toPtr) {
		writeJSONError(w, http.StatusBadRequest, "from must be before to")
		return
	}
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid before, want RFC3339")
			return
		}
		utc := t.UTC()
		beforePtr = &utc
	}
	var beforeIDPtr *int64
	if v := q.Get("before_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid before_id")
			return
		}
		beforeIDPtr = &id
		if beforePtr == nil {
			writeJSONError(w, http.StatusBadRequest, "before_id requires before")
			return
		}
	}
	kind := q.Get("kind")
	if kind != "" && kind != "all" {
		switch kind {
		case "drain", "agent", "runtime", "node", "warmup", "schedule", "predictive", "config":
		default:
			writeJSONError(w, http.StatusBadRequest, "invalid kind, want drain|agent|runtime|node|warmup|schedule|predictive|config|all")
			return
		}
		if kind == "predictive" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]store.SystemAuditEntry{})
			return
		}
	}
	if kind == "all" {
		kind = ""
	}
	action := q.Get("action")
	username := q.Get("user")
	target := q.Get("target")
	sourceIP := q.Get("source_ip")
	// If any enterprise filter is present, use filtered query for correct
	// pagination and index use. Otherwise keep old simple path for backward
	// compat and to avoid extra query planning overhead.
	hasFilter := fromPtr != nil || toPtr != nil || beforePtr != nil || beforeIDPtr != nil || kind != "" || action != "" || username != "" || target != "" || sourceIP != ""
	var entries []store.SystemAuditEntry
	var err error
	if hasFilter {
		entries, err = s.st.QuerySystemAuditLogFiltered(store.SystemAuditFilter{
			From:     fromPtr,
			To:       toPtr,
			Before:   beforePtr,
			BeforeID: beforeIDPtr,
			Limit:    limit,
			Kind:     kind,
			Action:   action,
			Username: username,
			Target:   target,
			SourceIP: sourceIP,
		})
	} else {
		entries, err = s.st.QuerySystemAuditLog(limit)
	}
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

// backupFilenameRE matches exactly the marbor-backup-<UTC timestamp>.db shape
// produced by backupFilename - used both to list scheduled backups and to
// validate a client-supplied filename before restore ever touches disk.
var backupFilenameRE = regexp.MustCompile(`^marbor-backup-\d{8}-\d{6}\.db$`)

// backupFileInfo is one entry in GET /admin/backup/list's response.
type backupFileInfo struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
}

// handleListBackups lists scheduled backup files already sitting in
// cfg.Backup.TargetDir, newest first, so the Settings restore picker doesn't
// require the operator to know the naming scheme or type a path by hand.
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	targetDir := s.cfg.Backup.TargetDir
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if targetDir == "" {
		json.NewEncoder(w).Encode(map[string]any{"backups": []backupFileInfo{}})
		return
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No backup has ever run yet - a normal, not-yet-configured
			// state, not an error.
			json.NewEncoder(w).Encode(map[string]any{"backups": []backupFileInfo{}})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("could not list backups: %v", err))
		return
	}
	var files []backupFileInfo
	for _, e := range entries {
		if e.IsDir() || !backupFilenameRE.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, backupFileInfo{Name: e.Name(), SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name }) // timestamp-named, so lexical desc = newest first
	json.NewEncoder(w).Encode(map[string]any{"backups": files})
}

// handleRestoreBackup validates a one-click restore request and, if valid,
// hands the full path off to main.go via restoreCh - it never touches the
// live database or exits the process itself (see SetRestoreChannel).
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	if s.restoreCh == nil {
		writeJSONError(w, http.StatusNotImplemented, "restore is not available in this run mode")
		return
	}
	var req struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// filepath.Base + the strict backupFilenameRE together rule out path
	// traversal (../, absolute paths, subdirectories): only a bare
	// marbor-backup-<timestamp>.db name already sitting directly in TargetDir
	// is ever accepted - never an arbitrary path the client supplies.
	name := filepath.Base(req.Filename)
	if name != req.Filename || !backupFilenameRE.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid backup filename")
		return
	}
	s.mu.RLock()
	targetDir := s.cfg.Backup.TargetDir
	s.mu.RUnlock()
	if targetDir == "" {
		writeJSONError(w, http.StatusBadRequest, "no backup target directory configured")
		return
	}
	fullPath := filepath.Join(targetDir, name)
	if _, err := os.Stat(fullPath); err != nil {
		writeJSONError(w, http.StatusNotFound, "backup file not found")
		return
	}
	if err := store.ValidateBackupFile(fullPath); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf("backup file failed validation: %v", err))
		return
	}

	select {
	case s.restoreCh <- fullPath:
	default:
		writeJSONError(w, http.StatusConflict, "a restore is already in progress")
		return
	}

	s.logSystemChange(r, "restore_backup", "global", fmt.Sprintf("Restore requested from %s - marbor restarting", name))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{"restarting": true, "file": name})
}

// handleBackupNow performs an on-demand backup (VACUUM INTO a temp file) and
// streams it back as a download - the manual counterpart to the scheduled
// job in StartBackupScheduler. Mirrors handleAnalyticsExport's
// Content-Disposition streaming pattern.
func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	tmp, err := os.CreateTemp("", "marbor-backup-download-*.db")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create temp file for backup")
		return
	}
	tmpPath := tmp.Name()
	tmp.Close()
	// VACUUM INTO refuses to write to a path that already exists.
	if err := os.Remove(tmpPath); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to prepare temp file for backup")
		return
	}
	defer os.Remove(tmpPath)

	if err := s.st.BackupTo(tmpPath); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("backup failed: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, backupFilename(time.Now())))
	http.ServeFile(w, r, tmpPath)
	s.logSystemChange(r, "manual_backup", "global", "Downloaded an on-demand marbor.db backup")
}

// maxUploadedBackupSize caps a browser-uploaded backup file to 2 GiB -
// generous for a marbor.db (typically low tens of MB) while still bounding
// disk/memory use against an abusive or mistaken upload.
const maxUploadedBackupSize = 2 << 30 // 2 GiB

// handleUploadBackup lets an operator attach an arbitrary local .db file
// (the "+" control next to the restore picker in Settings) and adds it to
// the same pool of restorable backups as scheduled/manual ones. The upload
// is staged to a temp file inside TargetDir first and validated with the
// same store.ValidateBackupFile (PRAGMA quick_check) used everywhere else in
// this feature, so a non-database file is rejected before it ever becomes a
// restore candidate. The staged file is then renamed to the standard
// marbor-backup-<timestamp>.db shape (backupFilename) so it passes
// backupFilenameRE and shows up in handleListBackups like any other backup -
// no separate "uploaded" code path for handleRestoreBackup to special-case.
func (s *Server) handleUploadBackup(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	targetDir := s.cfg.Backup.TargetDir
	s.mu.RUnlock()
	if targetDir == "" {
		writeJSONError(w, http.StatusBadRequest, "no backup target directory configured")
		return
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("create backup target directory: %v", err))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadedBackupSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid upload (file too large or malformed request)")
		return
	}
	// ParseMultipartForm spills any part over the 32MB threshold to its own
	// temp file under os.TempDir() - a marbor.db upload always does, since it's
	// well above 32MB. That spill file is separate from tmpPath below (which
	// this handler stages and cleans up itself) and net/http never removes
	// it on the success path, so without this it leaks a full-size temp file
	// on every single upload.
	defer r.MultipartForm.RemoveAll()
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()

	// Staged inside targetDir (not os.TempDir) so the final rename below is
	// an atomic same-filesystem move, not a cross-filesystem copy.
	tmp, err := os.CreateTemp(targetDir, "marbor-backup-upload-*.tmp")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to stage upload")
		return
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		writeJSONError(w, http.StatusBadRequest, "failed to read upload (too large or connection interrupted)")
		return
	}
	tmp.Close()

	if err := store.ValidateBackupFile(tmpPath); err != nil {
		os.Remove(tmpPath)
		writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf("not a valid marbor.db backup: %v", err))
		return
	}

	// If this upload is byte-for-byte identical to a backup already in the
	// pool (e.g. the operator re-uploading a file they just downloaded),
	// reuse the existing entry instead of adding a duplicate - report it as
	// a normal success so the caller selects the existing file, exactly as
	// it would a freshly uploaded one.
	hash, err := hashBackupFile(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		writeJSONError(w, http.StatusInternalServerError, "failed to check uploaded backup for duplicates")
		return
	}
	if dup, err := findDuplicateBackup(targetDir, hash); err == nil && dup != "" {
		os.Remove(tmpPath)
		s.logSystemChange(r, "upload_backup", "global", fmt.Sprintf("Uploaded backup matched existing %s - reused it instead of adding a duplicate", dup))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"filename": dup, "duplicate": true})
		return
	}

	// backupFilename is second-resolution; retry on the rare collision with
	// another upload or a scheduled backup landing in the same second rather
	// than ever overwriting an existing backup.
	var finalName, finalPath string
	var renameErr error
	saved := false
	for attempt := 0; attempt < 5; attempt++ {
		finalName = backupFilename(time.Now())
		finalPath = filepath.Join(targetDir, finalName)
		if _, err := os.Stat(finalPath); os.IsNotExist(err) {
			renameErr = os.Rename(tmpPath, finalPath)
			saved = renameErr == nil
			break
		}
		time.Sleep(time.Second)
	}
	if !saved {
		os.Remove(tmpPath)
		if renameErr != nil {
			// A rename failure here (permissions, disk full, cross-device)
			// is a fundamentally different problem than a name collision -
			// report the real cause rather than a misleading 409.
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save uploaded backup: %v", renameErr))
			return
		}
		writeJSONError(w, http.StatusConflict, "a backup with this timestamp already exists - please retry")
		return
	}

	// An uploaded file is added to the same pool scheduled backups prune from
	// - without this, repeated uploads could fill the disk with no cap even
	// while scheduled backups (with their own RetentionCount) stay bounded.
	s.mu.RLock()
	retentionCount := s.cfg.Backup.RetentionCount
	s.mu.RUnlock()
	s.pruneOldBackups(targetDir, retentionCount)

	s.logSystemChange(r, "upload_backup", "global", fmt.Sprintf("Uploaded a custom backup file as %s", finalName))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"filename": finalName})
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
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="marbor-models-%s.csv"`, today))
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
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="marbor-analytics-%s.csv"`, today))
			cw := csv.NewWriter(w)
			_ = cw.Write([]string{"hour", "local_requests", "cloud_requests", "saved_usd", "spent_usd"})
			for _, b := range s.analytics.last24hBuckets() {
				// b.Hour is UTC hour key "2006-01-02T15" - export as full RFC3339 Z
				// so downstream consumers have an unambiguous instant, not a bare wall hour.
				hourRFC3339 := b.Hour + ":00:00Z"
				_ = cw.Write([]string{
					hourRFC3339,
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
		buckets := s.analytics.last24hBuckets()
		// Transform bare hour keys to RFC3339 Z for an unambiguous wire (P393 U12)
		type exportBucket struct {
			Hour          string  `json:"hour"`
			Local         int64   `json:"local"`
			Cloud         int64   `json:"cloud"`
			SavedUSD      float64 `json:"saved_usd"`
			SpentUSD      float64 `json:"spent_usd"`
			Tokens        int64   `json:"tokens"`
			GenDurationMs int64   `json:"gen_duration_ms"`
			TokensPerSec  float64 `json:"tokens_per_sec"`
		}
		out := make([]exportBucket, len(buckets))
		for i, b := range buckets {
			out[i] = exportBucket{
				Hour: b.Hour + ":00:00Z", Local: b.Local, Cloud: b.Cloud,
				SavedUSD: b.SavedUSD, SpentUSD: b.SpentUSD, Tokens: b.Tokens,
				GenDurationMs: b.GenDurationMs, TokensPerSec: b.TokensPerSec,
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"hourly": out,
		})
	}
}

// vramAgentVendorToolLabel maps a Marbor Agent's detected GPU vendor
// (marboragent.GPUBlock.Vendor - "nvidia"/"rocm"/"intel"/"apple") to the actual
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
// and (when the source is "agent") the vendor the Marbor Agent detected. Only
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
		nodeRuntime := n.Runtime
		vramTotalMB := n.VRAMTotalMB
		agentGPUs := append([]marboragent.GPUInfo(nil), n.AgentGPUs...)
		declaredGPUIndices := append([]int(nil), n.DeclaredGPUIndices...)
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

		capacityMB, capacityUsedMB, _, _ := nodeVRAMCapacity(vramTotalMB, agentGPUs, nodeRuntime, declaredGPUIndices)
		if capacityMB > 0 {
			vramTotalBytes = capacityMB * 1024 * 1024
			// Use nvidia-smi total minus what /api/ps says is loaded (or, on
			// a multi-GPU node sized against a single device - see
			// nodeVRAMCapacity - that same device's own reported usage).
			usedMB := vramUsedMBFromPS
			if capacityUsedMB >= 0 {
				usedMB = capacityUsedMB
			}
			vramUsedBytes := usedMB * 1024 * 1024
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
			// Classify against total VRAM capacity, not currently-free VRAM -
			// free VRAM is a transient snapshot of whatever else is warm on
			// the node right now (see classifyFit in catalog.go).
			fit := "unknown"
			if vramSource != "unknown" && vramSource != "inferred" {
				switch {
				case estimate <= int64(float64(vramTotalBytes)*0.85):
					fit = "green"
				case estimate <= vramTotalBytes:
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

// safeStringMap returns a copy of m safe to serialize outside the caller's
// lock (m is read under NodeState.RLock, but json.Marshal can't hold that
// lock across the whole response encode).
func safeStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	totalMB, freeMB, ramOK := readSystemMemory()
	if !ramOK {
		totalMB, freeMB = 0, 0
	}

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
		RAMKnown:   ramOK,
		GPUs:       gpus,
		ServerTime: nowTime.Format("2006-01-02 15:04:05"),
		Timezone:   displayTz,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}
