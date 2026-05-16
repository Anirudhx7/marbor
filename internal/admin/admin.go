package admin

import (
	"embed"
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

	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

//go:embed web/dist
var webFS embed.FS

type Server struct {
	router     *router.Router
	auth       *auth.Middleware
	cfg        config.Config
	adminToken string
	mu         sync.RWMutex
	requests   []RequestLog
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
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/nodes", s.cors(s.adminAuth(s.handleNodes)))
	mux.HandleFunc("POST /admin/nodes", s.cors(s.adminAuth(s.handleAddNode)))
	mux.HandleFunc("DELETE /admin/nodes/{name}", s.cors(s.adminAuth(s.handleRemoveNode)))

	mux.HandleFunc("GET /admin/keys", s.cors(s.adminAuth(s.handleKeys)))
	mux.HandleFunc("POST /admin/keys", s.cors(s.adminAuth(s.handleAddKey)))
	mux.HandleFunc("DELETE /admin/keys/{name}", s.cors(s.adminAuth(s.handleRevokeKey)))

	mux.HandleFunc("GET /admin/routing/rules", s.cors(s.adminAuth(s.handleRoutingRules)))
	mux.HandleFunc("POST /admin/routing/rules", s.cors(s.adminAuth(s.handleAddRoutingRule)))
	mux.HandleFunc("DELETE /admin/routing/rules/{id}", s.cors(s.adminAuth(s.handleRemoveRoutingRule)))
	mux.HandleFunc("PUT /admin/routing/rules/{id}/toggle", s.cors(s.adminAuth(s.handleToggleRoutingRule)))
	mux.HandleFunc("PUT /admin/routing/strategy", s.cors(s.adminAuth(s.handleSetRoutingStrategy)))

	mux.HandleFunc("GET /admin/settings", s.cors(s.adminAuth(s.handleSettings)))
	mux.HandleFunc("PUT /admin/settings", s.cors(s.adminAuth(s.handleUpdateSettings)))

	mux.HandleFunc("GET /admin/requests/live", s.cors(s.adminAuth(s.handleLiveRequests)))
	mux.HandleFunc("GET /admin/metrics/summary", s.cors(s.adminAuth(s.handleSummary)))

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
