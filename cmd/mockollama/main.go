// cmd/mockollama/main.go - Mock Ollama HTTP server for demo and testing.
// Configurable via env vars: NODE_NAME, WARM_MODELS, ALL_MODELS, PORT, LATENCY_MS
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	stateMu    sync.RWMutex
	warmModels = make(map[string]bool)
	allModels  []string
)

type modelInfo struct {
	Name       string    `json:"name"`
	ModifiedAt time.Time `json:"modified_at"`
	Size       int64     `json:"size"`
	Digest     string    `json:"digest"`
}

type psModel struct {
	Name      string    `json:"name"`
	Model     string    `json:"model"`
	Size      int64     `json:"size"`
	SizeVram  int64     `json:"size_vram"`
	Digest    string    `json:"digest"`
	ExpiresAt time.Time `json:"expires_at"`
}

// modelSizes maps model names to realistic size_vram values (bytes).
var modelSizes = map[string]int64{
	"llama3.2:3b":   2_100_000_000,
	"llama3.2:8b":   4_700_000_000,
	"mistral:7b":    4_100_000_000,
	"qwen2.5:7b":    4_400_000_000,
	"qwen2.5:14b":   8_900_000_000,
	"codellama:13b": 7_300_000_000,
}

func sizeFor(model string) int64 {
	if s, ok := modelSizes[model]; ok {
		return s
	}
	return 3_800_000_000
}

func digestFor(model string) string {
	return fmt.Sprintf("sha256:%064x", len(model)*7+42)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitModels(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	nodeName := envOrDefault("NODE_NAME", "mock-node")
	for _, m := range splitModels(envOrDefault("WARM_MODELS", "llama3.2:3b,qwen2.5:7b")) {
		warmModels[m] = true
	}
	allModels = splitModels(envOrDefault("ALL_MODELS", "llama3.2:3b,qwen2.5:7b,mistral:7b"))
	port := envOrDefault("PORT", "11434")
	latencyMs, err := strconv.Atoi(envOrDefault("LATENCY_MS", "150"))
	if err != nil {
		latencyMs = 150
	}

	stateMu.RLock()
	var warmList []string
	for m, warm := range warmModels {
		if warm {
			warmList = append(warmList, m)
		}
	}
	stateMu.RUnlock()

	log.Printf("[%s] mock-ollama starting on :%s  warm=%v  all=%v  latency=%dms",
		nodeName, port, warmList, allModels, latencyMs)

	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "Ollama is running")
			return
		}
		http.NotFound(w, r)
	})

	// GET /api/tags - all installed models
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		models := make([]modelInfo, 0, len(allModels))
		for _, m := range allModels {
			models = append(models, modelInfo{
				Name:       m,
				ModifiedAt: time.Now().Add(-72 * time.Hour),
				Size:       sizeFor(m),
				Digest:     digestFor(m),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"models": models})
	})

	// GET /api/ps - currently loaded (warm) models
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		stateMu.RLock()
		ps := make([]psModel, 0, len(warmModels))
		for m, warm := range warmModels {
			if !warm {
				continue
			}
			sz := sizeFor(m)
			ps = append(ps, psModel{
				Name:      m,
				Model:     m,
				Size:      sz,
				SizeVram:  sz,
				Digest:    digestFor(m),
				ExpiresAt: time.Now().Add(5 * time.Minute),
			})
		}
		stateMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"models": ps})
	})

	// POST /api/generate - streaming NDJSON generate response
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleGenerate(w, r, nodeName, latencyMs)
	})

	// POST /api/chat - streaming NDJSON chat response
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleChat(w, r, nodeName, latencyMs)
	})

	// OpenAI-compat passthrough (mesh forwards /v1/* to Ollama)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"object":"list","data":[]}`)
	})

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("[%s] listen error: %v", nodeName, err)
	}
}

// responseTokens holds deterministic token sequences per model family.
var responseTokens = map[string][]string{
	"llama3.2": {"Hello", "!", " I", "'m", " a", " mock", " Llama", " 3.2", " node", ".", " I", " can", " help", " you", " with", " questions", " and", " tasks", "."},
	"mistral":  {"Bonjour", "!", " I", "'m", " a", " mock", " Mistral", " model", ".", " Ready", " to", " assist", " with", " any", " query", " you", " have", "."},
	"qwen2.5":  {"Hi", "!", " I", "'m", " mock", " Qwen", "2.5", ".", " Let", " me", " help", " you", " with", " that", " request", " efficiently", "."},
}

func tokensFor(model string) []string {
	for family, tokens := range responseTokens {
		if strings.HasPrefix(model, family) {
			return tokens
		}
	}
	return []string{"This", " is", " a", " mock", " response", " from", " node", "."}
}

func handleGenerate(w http.ResponseWriter, r *http.Request, nodeName string, latencyMs int) {
	var req struct {
		Model     string  `json:"model"`
		Prompt    string  `json:"prompt"`
		Stream    *bool   `json:"stream"`
		KeepAlive *string `json:"keep_alive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = "llama3.2:3b"
	}

	if req.KeepAlive != nil && (*req.KeepAlive == "0" || *req.KeepAlive == "0s") {
		stateMu.Lock()
		delete(warmModels, req.Model)
		stateMu.Unlock()
		log.Printf("[%s] Evicted model %q from VRAM via keep_alive=0s", nodeName, req.Model)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
		})
		return
	}

	stateMu.RLock()
	isWarm := warmModels[req.Model]
	stateMu.RUnlock()

	if !isWarm {
		coldDelay := 2000
		log.Printf("[%s] Cold start: loading model %q into VRAM (adding %dms delay)", nodeName, req.Model, coldDelay)
		time.Sleep(time.Duration(coldDelay) * time.Millisecond)
		stateMu.Lock()
		warmModels[req.Model] = true
		stateMu.Unlock()
	}

	tokens := tokensFor(req.Model)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Node-Name", nodeName)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	start := time.Now()
	time.Sleep(time.Duration(latencyMs) * time.Millisecond)

	promptEvalCount := 8
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	for i, tok := range tokens {
		chunk := map[string]interface{}{
			"model":      req.Model,
			"created_at": time.Now().Format(time.RFC3339Nano),
			"response":   tok,
			"done":       false,
		}
		if i == 0 {
			chunk["prompt_eval_count"] = promptEvalCount
		}
		enc.Encode(chunk)
		flusher.Flush()
		time.Sleep(15 * time.Millisecond)
	}

	evalCount := len(tokens)
	totalDuration := time.Since(start).Nanoseconds()
	final := map[string]interface{}{
		"model":             req.Model,
		"created_at":        time.Now().Format(time.RFC3339Nano),
		"response":          "",
		"done":              true,
		"done_reason":       "stop",
		"eval_count":        evalCount,
		"prompt_eval_count": promptEvalCount,
		"total_duration":    totalDuration,
		"load_duration":     int64(latencyMs) * 1_000_000,
		"eval_duration":     totalDuration - int64(latencyMs)*1_000_000,
	}
	enc.Encode(final)
	flusher.Flush()
}

func handleChat(w http.ResponseWriter, r *http.Request, nodeName string, latencyMs int) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream    *bool   `json:"stream"`
		KeepAlive *string `json:"keep_alive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = "llama3.2:3b"
	}

	if req.KeepAlive != nil && (*req.KeepAlive == "0" || *req.KeepAlive == "0s") {
		stateMu.Lock()
		delete(warmModels, req.Model)
		stateMu.Unlock()
		log.Printf("[%s] Evicted model %q from VRAM via keep_alive=0s", nodeName, req.Model)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
		})
		return
	}

	stateMu.RLock()
	isWarm := warmModels[req.Model]
	stateMu.RUnlock()

	if !isWarm {
		coldDelay := 2000
		log.Printf("[%s] Cold start: loading model %q into VRAM (adding %dms delay)", nodeName, req.Model, coldDelay)
		time.Sleep(time.Duration(coldDelay) * time.Millisecond)
		stateMu.Lock()
		warmModels[req.Model] = true
		stateMu.Unlock()
	}

	tokens := tokensFor(req.Model)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Node-Name", nodeName)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	start := time.Now()
	time.Sleep(time.Duration(latencyMs) * time.Millisecond)

	promptEvalCount := 12
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	for _, tok := range tokens {
		chunk := map[string]interface{}{
			"model":      req.Model,
			"created_at": time.Now().Format(time.RFC3339Nano),
			"message": map[string]string{
				"role":    "assistant",
				"content": tok,
			},
			"done": false,
		}
		enc.Encode(chunk)
		flusher.Flush()
		time.Sleep(15 * time.Millisecond)
	}

	evalCount := len(tokens)
	totalDuration := time.Since(start).Nanoseconds()
	final := map[string]interface{}{
		"model":      req.Model,
		"created_at": time.Now().Format(time.RFC3339Nano),
		"message": map[string]string{
			"role":    "assistant",
			"content": "",
		},
		"done":              true,
		"done_reason":       "stop",
		"eval_count":        evalCount,
		"prompt_eval_count": promptEvalCount,
		"total_duration":    totalDuration,
		"load_duration":     int64(latencyMs) * 1_000_000,
		"eval_duration":     totalDuration - int64(latencyMs)*1_000_000,
	}
	enc.Encode(final)
	flusher.Flush()
}
