package router

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

type ModelInfo struct {
	Name     string `json:"name"`
	SizeVRAM int64  `json:"sizeVram"`
}

type NodeState struct {
	Name          string
	URL           string
	GPUModel      string
	NvidiaIndex   int
	LoadedModels  []ModelInfo
	ActiveConns   int32
	Healthy       bool
	LastPollAt    time.Time
	Failures      int
	CPUPercent    float64
	Temperature   *float64
	VRAMTotalMB   int64
	VRAMUsedMB    int64
	PowerDrawW    float64
	Uptime        string
	HealthHistory []float64
	FirstSeenAt   time.Time
	mu            sync.RWMutex
}

// TagsCache holds a cached result from /api/tags for a single node.
type TagsCache struct {
	Models    []TagModel
	FetchedAt time.Time
}

// TagModel represents one model entry from /api/tags.
type TagModel struct {
	Name string `json:"name"`
	Size int64  `json:"size"` // bytes on disk
}

// tagsInflightEntry represents an in-progress /api/tags fetch.
type tagsInflightEntry struct {
	done   chan struct{}
	models []TagModel
	err    error
}

type Router struct {
	nodes          []*NodeState
	strategy       string
	fallback       string
	interval       time.Duration
	client         *http.Client
	mu             sync.RWMutex
	roundRobin     uint32
	rules          []config.RoutingRule
	clouds         []config.CloudProvider
	dockerCfg      config.DockerConfig
	webhookCfg     config.WebhookConfig
	discoveredURLs map[string]struct{} // URLs added via Docker discovery
	// prevHealthy tracks the last known health state per node name for
	// transition detection (healthy -> unhealthy and back).
	prevHealthy map[string]bool
	// tagsCache caches /api/tags results per node URL for 30 seconds.
	tagsCache    map[string]*TagsCache
	tagsMu       sync.Mutex
	// tagsInflight prevents concurrent fetches to the same node URL (cache stampede).
	tagsInflight map[string]*tagsInflightEntry
}

func New(cfg config.RoutingConfig, nodesCfg []config.NodeConfig, clouds []config.CloudProvider) *Router {
	nodes := make([]*NodeState, len(nodesCfg))
	for i, n := range nodesCfg {
		nodes[i] = &NodeState{
			Name:        n.Name,
			URL:         n.URL,
			GPUModel:    n.GPUModel,
			NvidiaIndex: n.NvidiaIndex,
			Healthy:     true,
			FirstSeenAt: time.Now(),
		}
	}
	cloudsCopy := make([]config.CloudProvider, len(clouds))
	copy(cloudsCopy, clouds)
	prev := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		prev[n.Name] = true // assume healthy at start
	}
	return &Router{
		nodes:          nodes,
		strategy:       cfg.Strategy,
		fallback:       cfg.Fallback,
		interval:       time.Duration(cfg.PollIntervalMs) * time.Millisecond,
		client:         &http.Client{Timeout: 5 * time.Second},
		rules:          cfg.Rules,
		clouds:         cloudsCopy,
		discoveredURLs: make(map[string]struct{}),
		prevHealthy:    prev,
		tagsCache:      make(map[string]*TagsCache),
		tagsInflight:   make(map[string]*tagsInflightEntry),
	}
}

func (r *Router) SetDockerConfig(cfg config.DockerConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dockerCfg = cfg
}

func (r *Router) SetWebhookConfig(cfg config.WebhookConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.webhookCfg = cfg
}

// fireWebhook sends a node-state-change notification to the configured webhook
// URL in a goroutine. Errors are logged but never propagate to the caller.
func (r *Router) fireWebhook(event, nodeName, nodeURL string) {
	r.mu.RLock()
	cfg := r.webhookCfg
	r.mu.RUnlock()
	if !cfg.Enabled || cfg.URL == "" {
		return
	}
	go func() {
		payload := map[string]string{
			"event": event,
			"node":  nodeName,
			"url":   nodeURL,
			"time":  time.Now().UTC().Format(time.RFC3339),
		}
		body, err := json.Marshal(payload)
		if err != nil {
			log.Printf("webhook: marshal payload: %v", err)
			return
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.URL, bytes.NewReader(body))
		if err != nil {
			log.Printf("webhook: create request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if cfg.Secret != "" {
			mac := hmac.New(sha256.New, []byte(cfg.Secret))
			mac.Write(body)
			sig := hex.EncodeToString(mac.Sum(nil))
			req.Header.Set("X-Ollama-Mesh-Signature", fmt.Sprintf("sha256=%s", sig))
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("webhook: post %s: %v", strings.TrimRight(cfg.URL, "/"), err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.Printf("webhook: server returned %d for event %s", resp.StatusCode, event)
		}
	}()
}

func (r *Router) Rules() []config.RoutingRule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]config.RoutingRule(nil), r.rules...)
}

func (r *Router) AddRule(rule config.RoutingRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append(r.rules, rule)
}

func (r *Router) RemoveRule(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, rule := range r.rules {
		if rule.ID == id {
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			break
		}
	}
}

func (r *Router) ToggleRule(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, rule := range r.rules {
		if rule.ID == id {
			r.rules[i].Enabled = !r.rules[i].Enabled
			break
		}
	}
}

func (r *Router) SetStrategy(strategy string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.strategy = strategy
}

// Strategy returns the current routing strategy.
func (r *Router) Strategy() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.strategy
}

// RouteCloud returns the first enabled cloud provider as fallback when no local nodes are available.
func (r *Router) RouteCloud() *config.CloudProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.clouds {
		if r.clouds[i].Enabled {
			return &r.clouds[i]
		}
	}
	return nil
}

func (r *Router) SetClouds(providers []config.CloudProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clouds = make([]config.CloudProvider, len(providers))
	copy(r.clouds, providers)
}

func (n *NodeState) Lock() {
	n.mu.Lock()
}

func (n *NodeState) Unlock() {
	n.mu.Unlock()
}

func (n *NodeState) RLock() {
	n.mu.RLock()
}

func (n *NodeState) RUnlock() {
	n.mu.RUnlock()
}

func (r *Router) Start(ctx context.Context) {
	r.pollAll()
	r.discoverAndAddDockerNodes()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	dockerInterval := 30 * time.Second
	r.mu.RLock()
	if r.dockerCfg.PollIntervalMs > 0 {
		dockerInterval = time.Duration(r.dockerCfg.PollIntervalMs) * time.Millisecond
	}
	r.mu.RUnlock()
	dockerTicker := time.NewTicker(dockerInterval)
	defer dockerTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.pollAll()
		case <-dockerTicker.C:
			r.discoverAndAddDockerNodes()
		}
	}
}

// discoverAndAddDockerNodes queries the Docker socket and adds any new
// Ollama containers as nodes. Already-known URLs are skipped.
func (r *Router) discoverAndAddDockerNodes() {
	r.mu.RLock()
	enabled := r.dockerCfg.Enabled
	socket := r.dockerCfg.Socket
	r.mu.RUnlock()
	if !enabled {
		return
	}

	found, err := discoverDockerNodes(socket)
	if err != nil {
		// Docker not available or socket missing — log silently, don't crash.
		return
	}
	for _, n := range found {
		r.mu.Lock()
		_, exists := r.discoveredURLs[n.URL]
		if !exists {
			r.discoveredURLs[n.URL] = struct{}{}
		}
		r.mu.Unlock()
		if !exists {
			r.AddNode(n)
		}
	}
}

func (r *Router) pollAll() {
	var wg sync.WaitGroup
	for _, n := range r.nodes {
		wg.Add(1)
		go func(node *NodeState) {
			defer wg.Done()
			r.pollNode(node)
		}(n)
	}
	wg.Wait()
}

func (r *Router) pollNode(n *NodeState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", n.URL+"/api/ps", nil)
	if err != nil {
		r.markFailure(n)
		return
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.markFailure(n)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.markFailure(n)
		return
	}
	var ps struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		r.markFailure(n)
		return
	}

	gpu, hasGPU := queryGPU(n.NvidiaIndex)

	n.mu.Lock()
	n.LoadedModels = ps.Models
	n.Healthy = true
	n.Failures = 0
	n.LastPollAt = time.Now()
	n.Uptime = formatUptime(time.Since(n.FirstSeenAt))
	if hasGPU {
		n.VRAMTotalMB = gpu.VRAMTotalMB
		n.VRAMUsedMB = gpu.VRAMUsedMB
		n.PowerDrawW = gpu.PowerDrawW
		temp := gpu.TempCelsius
		n.Temperature = &temp
	}
	n.HealthHistory = append(n.HealthHistory, 100.0)
	if len(n.HealthHistory) > 60 {
		n.HealthHistory = n.HealthHistory[len(n.HealthHistory)-60:]
	}
	nodeName := n.Name
	nodeURL := n.URL
	n.mu.Unlock()
	metrics.NodeHealthy(n.Name, 1)

	// Fire webhook on recovery (unhealthy -> healthy transition).
	r.mu.Lock()
	prev, seen := r.prevHealthy[nodeName]
	if seen && !prev {
		r.prevHealthy[nodeName] = true
		r.mu.Unlock()
		r.fireWebhook("node_up", nodeName, nodeURL)
	} else {
		if !seen {
			r.prevHealthy[nodeName] = true
		}
		r.mu.Unlock()
	}
}

func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	return fmt.Sprintf("%dh", hours)
}

func (r *Router) markFailure(n *NodeState) {
	n.mu.Lock()
	n.Failures++
	becameUnhealthy := false
	if n.Failures >= 3 && n.Healthy {
		n.Healthy = false
		becameUnhealthy = true
		metrics.NodeHealthy(n.Name, 0)
	} else if n.Failures >= 3 {
		metrics.NodeHealthy(n.Name, 0)
	}
	healthScore := 0.0
	n.HealthHistory = append(n.HealthHistory, healthScore)
	if len(n.HealthHistory) > 60 {
		n.HealthHistory = n.HealthHistory[len(n.HealthHistory)-60:]
	}
	nodeName := n.Name
	nodeURL := n.URL
	n.mu.Unlock()

	if becameUnhealthy {
		// Fire webhook on node_down transition.
		r.mu.Lock()
		r.prevHealthy[nodeName] = false
		r.mu.Unlock()
		r.fireWebhook("node_down", nodeName, nodeURL)
	}
}

func (r *Router) Route(modelName string) (*NodeState, bool) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	strategy := r.strategy
	fallback := r.fallback
	r.mu.RUnlock()

	var healthy []*NodeState
	for _, n := range nodes {
		n.mu.RLock()
		isHealthy := n.Healthy
		n.mu.RUnlock()
		if isHealthy {
			healthy = append(healthy, n)
		}
	}
	if len(healthy) == 0 {
		return nil, false
	}

	if modelName != "" && strategy == "warm-first" {
		var warm []*NodeState
		for _, n := range healthy {
			n.mu.RLock()
			for _, m := range n.LoadedModels {
				if m.Name == modelName {
					warm = append(warm, n)
					break
				}
			}
			n.mu.RUnlock()
		}
		if len(warm) > 0 {
			metrics.CacheHit()
			return pickLeastConns(warm), true
		}
		metrics.CacheMiss()
	}

	if fallback == "least-connections" || fallback == "" {
		return pickLeastConns(healthy), false
	}
	if fallback == "round-robin" {
		idx := atomic.AddUint32(&r.roundRobin, 1) % uint32(len(healthy))
		return healthy[idx], false
	}
	return healthy[0], false
}

func pickLeastConns(nodes []*NodeState) *NodeState {
	var best *NodeState
	minConns := int32(math.MaxInt32)
	for _, n := range nodes {
		conns := atomic.LoadInt32(&n.ActiveConns)
		if conns < minConns {
			minConns = conns
			best = n
		}
	}
	return best
}

func (r *Router) AddNode(n config.NodeConfig) {
	node := &NodeState{
		Name:        n.Name,
		URL:         n.URL,
		GPUModel:    n.GPUModel,
		NvidiaIndex: n.NvidiaIndex,
		Healthy:     true,
		FirstSeenAt: time.Now(),
	}
	r.mu.Lock()
	r.nodes = append(r.nodes, node)
	r.mu.Unlock()
	// Start polling immediately
	go r.pollNode(node)
}

func (r *Router) RemoveNode(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, n := range r.nodes {
		if n.Name == name {
			r.nodes = append(r.nodes[:i], r.nodes[i+1:]...)
			break
		}
	}
}

func (r *Router) Nodes() []*NodeState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*NodeState, len(r.nodes))
	copy(out, r.nodes)
	return out
}

// NodeURLs returns a map of node name to URL for all configured nodes.
func (r *Router) NodeURLs() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m := make(map[string]string, len(r.nodes))
	for _, s := range r.nodes {
		m[s.Name] = s.URL
	}
	return m
}

func (r *Router) IncrConn(node *NodeState) {
	if node != nil {
		v := atomic.AddInt32(&node.ActiveConns, 1)
		metrics.ActiveConnections(node.Name, float64(v))
	}
}

func (r *Router) DecrConn(node *NodeState) {
	if node != nil {
		v := atomic.AddInt32(&node.ActiveConns, -1)
		if v < 0 {
			atomic.StoreInt32(&node.ActiveConns, 0)
			v = 0
		}
		metrics.ActiveConnections(node.Name, float64(v))
	}
}

func ExtractModelName(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err == nil && req.Model != "" {
		return req.Model
	}
	return ""
}

// FetchModelTags fetches /api/tags from a node and returns the list of
// downloaded models with their disk sizes. Results are cached for 30 seconds
// to avoid hammering nodes on every dashboard refresh.
// Concurrent callers for the same nodeURL share a single in-flight request
// (singleflight pattern) to prevent cache stampedes.
func (r *Router) FetchModelTags(nodeURL string) ([]TagModel, error) {
	const cacheTTL = 30 * time.Second

	r.tagsMu.Lock()
	// Serve from cache if fresh.
	if cached, ok := r.tagsCache[nodeURL]; ok && time.Since(cached.FetchedAt) < cacheTTL {
		models := make([]TagModel, len(cached.Models))
		copy(models, cached.Models)
		r.tagsMu.Unlock()
		return models, nil
	}
	// Deduplicate concurrent fetches to the same node.
	if inflight, ok := r.tagsInflight[nodeURL]; ok {
		r.tagsMu.Unlock()
		<-inflight.done
		if inflight.err != nil {
			return nil, inflight.err
		}
		models := make([]TagModel, len(inflight.models))
		copy(models, inflight.models)
		return models, nil
	}
	entry := &tagsInflightEntry{done: make(chan struct{})}
	r.tagsInflight[nodeURL] = entry
	r.tagsMu.Unlock()

	// This goroutine is the sole fetcher; all others wait on entry.done.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", nodeURL+"/api/tags", nil)
	if err != nil {
		entry.err = fmt.Errorf("build request: %w", err)
		r.tagsMu.Lock()
		delete(r.tagsInflight, nodeURL)
		r.tagsMu.Unlock()
		close(entry.done)
		return nil, entry.err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		entry.err = fmt.Errorf("fetch tags from %s: %w", nodeURL, err)
		r.tagsMu.Lock()
		delete(r.tagsInflight, nodeURL)
		r.tagsMu.Unlock()
		close(entry.done)
		return nil, entry.err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		entry.err = fmt.Errorf("tags %s: status %d", nodeURL, resp.StatusCode)
		r.tagsMu.Lock()
		delete(r.tagsInflight, nodeURL)
		r.tagsMu.Unlock()
		close(entry.done)
		return nil, entry.err
	}

	var tagsResp struct {
		Models []TagModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err != nil {
		entry.err = fmt.Errorf("decode tags: %w", err)
		r.tagsMu.Lock()
		delete(r.tagsInflight, nodeURL)
		r.tagsMu.Unlock()
		close(entry.done)
		return nil, entry.err
	}

	entry.models = tagsResp.Models
	r.tagsMu.Lock()
	r.tagsCache[nodeURL] = &TagsCache{
		Models:    tagsResp.Models,
		FetchedAt: time.Now(),
	}
	delete(r.tagsInflight, nodeURL)
	r.tagsMu.Unlock()
	close(entry.done)

	models := make([]TagModel, len(tagsResp.Models))
	copy(models, tagsResp.Models)
	return models, nil
}
