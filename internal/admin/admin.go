package admin

import (
	"bytes"
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

//go:embed web/dist
var webFS embed.FS

type Server struct {
	router        *router.Router
	auth          *auth.Middleware
	cfg           config.Config
	adminToken    string
	mu            sync.RWMutex
	requests      []RequestLog
	localCount    int64   // atomic - requests served by local nodes
	cloudCount    int64   // atomic - requests forwarded to cloud
	cloudSpentUSD float64 // protected by mu
	startTime     time.Time
	analytics     *analyticsStore
	auditLog      *audit.Logger
}

// SetAuditLogger wires the audit logger into the admin server for query access.
func (s *Server) SetAuditLogger(al *audit.Logger) {
	s.auditLog = al
}

type RequestLog struct {
	ID        string    `json:"id"`
	ApiKey    string    `json:"apiKey"`
	Model     string    `json:"model"`
	Node      string    `json:"node"`
	Status    string    `json:"status"`
	Latency   int       `json:"latency"`
	Time      time.Time `json:"time"`
}

type nodeResp struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Port          int                `json:"port"`
	GPUModel      string             `json:"gpuModel"`
	VRAMTotalMB   int64              `json:"vramTotalMB"`
	VRAMUsedMB    int64              `json:"vramUsedMB"`
	PowerDrawW    float64            `json:"powerDrawW"`
	CPUPercent    float64            `json:"cpuPercent"`
	Temperature   *float64           `json:"temperature"`
	Health        string             `json:"health"`
	Uptime        string             `json:"uptime"`
	LoadedModels  []router.ModelInfo `json:"loadedModels"`
	ActiveConns   int32              `json:"activeConns"`
	HealthHistory []float64          `json:"healthHistory"`
}

type keyResp struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Key              string   `json:"key"`
	Created          string   `json:"created"`
	RequestsToday    int      `json:"requestsToday"`
	RequestsThisMonth int     `json:"requestsThisMonth"`
	RateLimit        int      `json:"rateLimit"`
	Status           string   `json:"status"`
	AllowedModels    []string `json:"allowedModels"`
	ExpiresAt        string   `json:"expiresAt,omitempty"`
}

func NewServer(r *router.Router, a *auth.Middleware, cfg config.Config) *Server {
	token := "admin"
	if cfg.Auth.Enabled && len(cfg.Auth.Keys) > 0 {
		token = cfg.Auth.Keys[0].Key
	}
	return &Server{
		router:     r,
		auth:       a,
		cfg:        cfg,
		adminToken: token,
		startTime:  time.Now(),
		analytics:  newAnalyticsStore(),
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
	reg("POST /admin/nodes", s.cors(s.adminAuth(s.handleAddNode)))
	reg("DELETE /admin/nodes/{name}", s.cors(s.adminAuth(s.handleRemoveNode)))

	reg("GET /admin/keys", s.cors(s.adminAuth(s.handleKeys)))
	reg("POST /admin/keys", s.cors(s.adminAuth(s.handleAddKey)))
	reg("DELETE /admin/keys/{name}", s.cors(s.adminAuth(s.handleRevokeKey)))

	reg("GET /admin/routing/rules", s.cors(s.adminAuth(s.handleRoutingRules)))
	reg("POST /admin/routing/rules", s.cors(s.adminAuth(s.handleAddRoutingRule)))
	reg("DELETE /admin/routing/rules/{id}", s.cors(s.adminAuth(s.handleRemoveRoutingRule)))
	reg("PUT /admin/routing/rules/{id}/toggle", s.cors(s.adminAuth(s.handleToggleRoutingRule)))
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
	reg("GET /admin/audit", s.cors(s.adminAuth(s.handleAudit)))

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
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
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
		if token != s.adminToken {
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
		hist := make([]float64, len(n.HealthHistory))
		copy(hist, n.HealthHistory)
		if len(hist) == 0 {
			// Generate fake 60-point history if empty
			hist = make([]float64, 60)
			for j := range hist {
				hist[j] = 60.0 + float64(j%30)
			}
		}
		out[i] = nodeResp{
			ID:          fmt.Sprintf("gpu-%d", i),
			Name:        n.Name,
			Port:        port,
			GPUModel:    n.GPUModel,
			VRAMTotalMB: n.VRAMTotalMB,
			VRAMUsedMB:  n.VRAMUsedMB,
			PowerDrawW:  n.PowerDrawW,
			CPUPercent:    n.CPUPercent,
			Temperature:   n.Temperature,
			Health:        health,
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

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	out := make([]keyResp, 0, len(s.cfg.Auth.Keys))
	for i, k := range s.cfg.Auth.Keys {
		status := "active"
		today, month, models, expires, rateLimit, createdAt, ok := 0, 0, []string(nil), "", k.RateLimit, time.Time{}, false
		if s.auth != nil {
			today, month, models, expires, rateLimit, createdAt, ok = s.auth.KeyStats(k.Name)
		}
		if !ok {
			models = k.Models
			expires = k.ExpiresAt
			rateLimit = k.RateLimit
		}
		created := ""
		if !createdAt.IsZero() {
			created = createdAt.Format(time.RFC3339)
		}
		out = append(out, keyResp{
			ID:                fmt.Sprintf("key-%d", i+1),
			Name:              k.Name,
			Key:               k.Key,
			Created:           created,
			RequestsToday:     today,
			RequestsThisMonth: month,
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
		if n.Healthy {
			online++
		}
		totalConns += atomic.LoadInt32(&n.ActiveConns)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active_requests": totalConns,
		"nodes_online":    online,
		"total_nodes":     len(nodes),
	})
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

func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
	var k config.KeyConfig
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if k.Key == "" {
		k.Key = fmt.Sprintf("sk-%s-%d", k.Name, time.Now().UnixNano())
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.cfg)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.cfg = cfg
	// Save to config.yaml
	if err := config.SaveConfig("config.yaml", s.cfg); err != nil {
		http.Error(w, fmt.Sprintf("failed to save config: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) LogRequest(apiKey, model, node, status string, latencyMs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, RequestLog{
		ID:      fmt.Sprintf("req-%d", time.Now().UnixNano()),
		ApiKey:  apiKey,
		Model:   model,
		Node:    node,
		Status:  status,
		Latency: latencyMs,
		Time:    time.Now(),
	})
	if len(s.requests) > 50 {
		s.requests = s.requests[len(s.requests)-50:]
	}
}

func (s *Server) TrackLocalRequest() {
	atomic.AddInt64(&s.localCount, 1)
}

func (s *Server) TrackCloudCost(costPer1KTokens float64) {
	atomic.AddInt64(&s.cloudCount, 1)
	// Estimate 500 tokens per request as baseline for cost calculation
	cost := costPer1KTokens * 500.0 / 1000.0
	s.mu.Lock()
	s.cloudSpentUSD += cost
	s.mu.Unlock()
}

// TrackLocalRequestModel tracks a local request with model-level granularity.
// Call this alongside TrackLocalRequest() when the model name is available.
func (s *Server) TrackLocalRequestModel(model string) {
	atomic.AddInt64(&s.localCount, 1)
	s.analytics.recordLocal(model)
}

// TrackCloudCostModel tracks a cloud request with model-level granularity.
// Call this alongside TrackCloudCost() when the model name is available.
func (s *Server) TrackCloudCostModel(model string, costPer1K float64) {
	atomic.AddInt64(&s.cloudCount, 1)
	cost := costPer1K * 500.0 / 1000.0
	s.mu.Lock()
	s.cloudSpentUSD += cost
	s.mu.Unlock()
	s.analytics.recordCloud(model, costPer1K)
}

func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	hourly := s.analytics.last24hBuckets()
	models := s.analytics.topModels()

	local := atomic.LoadInt64(&s.localCount)
	cloud := atomic.LoadInt64(&s.cloudCount)
	s.mu.RLock()
	cloudSpent := s.cloudSpentUSD
	s.mu.RUnlock()
	const refCostPer1K = 0.002
	savedUSD := float64(local) * refCostPer1K * 500.0 / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"hourly":           hourly,
		"by_model":         models,
		"total_saved_usd":  savedUSD,
		"total_spent_usd":  cloudSpent,
		"local_requests":   local,
		"cloud_requests":   cloud,
	})
}

func (s *Server) handleSavings(w http.ResponseWriter, r *http.Request) {
	local := atomic.LoadInt64(&s.localCount)
	cloud := atomic.LoadInt64(&s.cloudCount)
	s.mu.RLock()
	cloudSpent := s.cloudSpentUSD
	s.mu.RUnlock()

	// Savings = what local requests would have cost at reference cloud rate
	// Using $0.002 per 1K tokens with 500 token average per request
	const refCostPer1K = 0.002
	savedUSD := float64(local) * refCostPer1K * 500.0 / 1000.0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"local_requests":   local,
		"cloud_requests":   cloud,
		"cloud_spent_usd":  cloudSpent,
		"saved_usd":        savedUSD,
		"total_requests":   local + cloud,
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
	out := make([]providerResp, 0, len(s.cfg.CloudProviders))
	for _, cp := range s.cfg.CloudProviders {
		out = append(out, providerResp{
			Name:            cp.Name,
			Provider:        cp.Provider,
			BaseURL:         cp.BaseURL,
			DefaultModel:    cp.DefaultModel,
			CostPer1KTokens: cp.CostPer1KTokens,
			Enabled:         cp.Enabled,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
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

// handleHealth is an unauthenticated endpoint for load balancers and Docker healthchecks.
// Returns 200 OK with node health summary and server uptime.
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

	w.Header().Set("Content-Type", "application/json")
	if status == "degraded" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         status,
		"version":        "0.1.0",
		"uptime_seconds": uptimeSecs,
		"nodes": map[string]int{
			"total":   total,
			"healthy": healthy,
		},
	})
}
