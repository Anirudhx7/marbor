// cmd/mocknode/main.go - Mock inference-node HTTP server for the demo stack
// and multi-runtime testing. RUNTIME selects which backend it impersonates:
// "ollama" (default, unchanged from this tool's original mockollama name),
// "vllm", "tgi", "llamacpp", or "mlx". Configurable via env vars: RUNTIME,
// NODE_NAME, MODEL_ID, WARM_MODELS, ALL_MODELS, PORT, LATENCY_MS.
//
// The non-Ollama runtimes deliberately implement only what marbor
// itself actually calls (internal/runtime's detect/health probes, verified
// against that package's source and tests) rather than each project's full
// real API surface - this is a mock of the marbor's integration contract, not
// a general-purpose vLLM/TGI/llama.cpp/MLX simulator:
//   - vllm/llamacpp: GET /health, GET /v1/models (owned_by:"vllm" is what
//     distinguishes vllm from llamacpp during auto-detection), POST
//     /v1/chat/completions, POST /v1/completions.
//   - tgi: GET /health, GET /info ({"model_id":...} is the only field the
//     marbor reads), POST /v1/chat/completions.
//   - mlx: GET /v1/models only (no /health route - matches real
//     mlx_lm.server, and is exactly what internal/runtime.MLXProbe checks),
//     POST /v1/chat/completions, POST /v1/completions.
//   - None of these implement /api/tags: marbor already treats its
//     absence as an expected, gracefully-degraded case (see
//     internal/router/eviction.go's estimateModelSizeBytes comment).
package main

import (
	"bufio"
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
	runtime := strings.ToLower(envOrDefault("RUNTIME", "ollama"))
	nodeName := envOrDefault("NODE_NAME", "mock-node")
	port := envOrDefault("PORT", "11434")
	latencyMs, err := strconv.Atoi(envOrDefault("LATENCY_MS", "150"))
	if err != nil {
		latencyMs = 150
	}

	switch runtime {
	case "vllm", "tgi", "llamacpp", "mlx":
		runOpenAICompatMock(runtime, nodeName, port, latencyMs)
	default:
		runOllamaMock(nodeName, port, latencyMs)
	}
}

// --- Ollama mock (unchanged behavior from this tool's original name) ---

func runOllamaMock(nodeName, port string, latencyMs int) {
	for _, m := range splitModels(envOrDefault("WARM_MODELS", "llama3.2:3b,qwen2.5:7b")) {
		warmModels[m] = true
	}
	allModels = splitModels(envOrDefault("ALL_MODELS", "llama3.2:3b,qwen2.5:7b,mistral:7b"))

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

	// OpenAI-compat passthrough (marbor forwards /v1/* to Ollama)
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
	"llama-3":  {"Hi", "!", " I", "'m", " a", " mock", " Llama", " 3", " model", " served", " through", " an", " OpenAI-compatible", " endpoint", "."},
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
		_, wasWarm := warmModels[req.Model]
		if wasWarm {
			delete(warmModels, req.Model)
		}
		stateMu.Unlock()
		if wasWarm {
			log.Printf("[%s] Evicted model %q from VRAM via keep_alive=0s", nodeName, req.Model)
		} else {
			log.Printf("[%s] keep_alive=0s for model %q, which was not warm - no-op", nodeName, req.Model)
		}
		// Real Ollama returns the standard generate-then-unload done-chunk
		// shape here (done_reason "unload"), not a nonstandard
		// {"status":"success"} object.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":             req.Model,
			"created_at":        time.Now().Format(time.RFC3339Nano),
			"response":          "",
			"done":              true,
			"done_reason":       "unload",
			"eval_count":        0,
			"prompt_eval_count": 0,
			"total_duration":    0,
			"load_duration":     0,
			"eval_duration":     0,
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
	w.Header().Set("X-Node-Name", nodeName)

	start := time.Now()
	time.Sleep(time.Duration(latencyMs) * time.Millisecond)

	promptEvalCount := 8

	if req.Stream != nil && !*req.Stream {
		// Ollama's stream:false contract returns one JSON object, not NDJSON.
		evalCount := len(tokens)
		totalDuration := time.Since(start).Nanoseconds()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":             req.Model,
			"created_at":        time.Now().Format(time.RFC3339Nano),
			"response":          strings.Join(tokens, ""),
			"done":              true,
			"done_reason":       "stop",
			"eval_count":        evalCount,
			"prompt_eval_count": promptEvalCount,
			"total_duration":    totalDuration,
			"load_duration":     int64(latencyMs) * 1_000_000,
			"eval_duration":     totalDuration - int64(latencyMs)*1_000_000,
		})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

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
		_, wasWarm := warmModels[req.Model]
		if wasWarm {
			delete(warmModels, req.Model)
		}
		stateMu.Unlock()
		if wasWarm {
			log.Printf("[%s] Evicted model %q from VRAM via keep_alive=0s", nodeName, req.Model)
		} else {
			log.Printf("[%s] keep_alive=0s for model %q, which was not warm - no-op", nodeName, req.Model)
		}
		// Real Ollama returns the standard chat done-chunk shape here
		// (done_reason "unload"), not a nonstandard {"status":"success"}
		// object.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":             req.Model,
			"created_at":        time.Now().Format(time.RFC3339Nano),
			"message":           map[string]string{"role": "assistant", "content": ""},
			"done":              true,
			"done_reason":       "unload",
			"eval_count":        0,
			"prompt_eval_count": 0,
			"total_duration":    0,
			"load_duration":     0,
			"eval_duration":     0,
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
	w.Header().Set("X-Node-Name", nodeName)

	start := time.Now()
	time.Sleep(time.Duration(latencyMs) * time.Millisecond)

	promptEvalCount := 12

	if req.Stream != nil && !*req.Stream {
		// Ollama's stream:false contract returns one JSON object, not NDJSON.
		evalCount := len(tokens)
		totalDuration := time.Since(start).Nanoseconds()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":      req.Model,
			"created_at": time.Now().Format(time.RFC3339Nano),
			"message": map[string]string{
				"role":    "assistant",
				"content": strings.Join(tokens, ""),
			},
			"done":              true,
			"done_reason":       "stop",
			"eval_count":        evalCount,
			"prompt_eval_count": promptEvalCount,
			"total_duration":    totalDuration,
			"load_duration":     int64(latencyMs) * 1_000_000,
			"eval_duration":     totalDuration - int64(latencyMs)*1_000_000,
		})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

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

// --- OpenAI-compatible mock: vllm / tgi / llamacpp ---

// defaultModelID returns a realistic single-model identity for a runtime
// that wasn't given an explicit MODEL_ID - these three runtimes are
// conventionally single-model-per-process, unlike Ollama's multi-model
// warm/cold catalog.
func defaultModelID(runtime string) string {
	switch runtime {
	case "vllm":
		return "meta-llama/Llama-3.1-8B-Instruct"
	case "tgi":
		return "mistralai/Mistral-7B-Instruct-v0.3"
	case "llamacpp":
		return "llama-3.2-3b-instruct.Q4_K_M.gguf"
	case "mlx":
		return "mlx-community/Llama-3.2-3B-Instruct-4bit"
	}
	return "mock-model"
}

func runOpenAICompatMock(runtime, nodeName, port string, latencyMs int) {
	modelID := envOrDefault("MODEL_ID", defaultModelID(runtime))

	log.Printf("[%s] mock-%s starting on :%s  model=%s  latency=%dms", nodeName, runtime, port, modelID, latencyMs)

	mux := http.NewServeMux()

	if runtime != "mlx" {
		// GET /health - liveness probe vllm/tgi/llamacpp all need
		// (internal/runtime's shared checkHealth requires a bare 200). mlx
		// is deliberately excluded: real mlx_lm.server has no /health route,
		// and internal/runtime.MLXProbe never calls one - only /v1/models.
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
		})
	}

	switch runtime {
	case "tgi":
		// GET /info - the only field internal/runtime/tgi.go reads is model_id.
		mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"model_id": modelID})
		})
	default: // vllm, llamacpp, mlx
		// GET /v1/models - owned_by:"vllm" is what internal/runtime/detect.go
		// uses to tell vllm and llamacpp apart; llamacpp must NOT send that
		// value. mlx is never auto-detected via this field (reached only by
		// explicit runtime:mlx config), so its own runtime name is fine here.
		ownedBy := runtime
		mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"object": "list",
				"data": []map[string]interface{}{
					{"id": modelID, "object": "model", "owned_by": ownedBy},
				},
			})
		})
	}

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		handleOpenAIChatCompletion(w, r, nodeName, modelID, latencyMs)
	})
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, r *http.Request) {
		handleOpenAICompletion(w, r, nodeName, modelID, latencyMs)
	})

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("[%s] listen error: %v", nodeName, err)
	}
}

// responseTokenFamilies is responseTokens' keys in a fixed order, so
// openAITokensFor's matching is deterministic (map iteration order is not).
var responseTokenFamilies = []string{"llama3.2", "mistral", "qwen2.5", "llama-3"}

func openAITokensFor(model string) []string {
	lower := strings.ToLower(model)
	compact := strings.ReplaceAll(lower, ".", "")
	for _, family := range responseTokenFamilies {
		fc := strings.ReplaceAll(family, ".", "")
		if strings.Contains(lower, family) || strings.Contains(compact, fc) {
			return responseTokens[family]
		}
	}
	return []string{"This", " is", " a", " mock", " OpenAI-compatible", " response", " from", " node", "."}
}

func handleOpenAIChatCompletion(w http.ResponseWriter, r *http.Request, nodeName, modelID string, latencyMs int) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream *bool `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = modelID
	}

	promptTokens := 12
	tokens := openAITokensFor(req.Model)
	// OpenAI's own default is non-streaming, but demos care most about
	// exercising the streaming path, so default true here unless the client
	// explicitly asked for stream:false.
	stream := req.Stream == nil || *req.Stream
	id := fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	time.Sleep(time.Duration(latencyMs) * time.Millisecond)
	w.Header().Set("X-Node-Name", nodeName)

	if !stream {
		full := strings.Join(tokens, "")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []map[string]interface{}{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": full}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": promptTokens, "completion_tokens": len(tokens), "total_tokens": promptTokens + len(tokens)},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	bw := bufio.NewWriter(w)

	writeChunk := func(delta map[string]interface{}, finishReason interface{}) {
		chunk := map[string]interface{}{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": delta, "finish_reason": finishReason},
			},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(bw, "data: %s\n\n", b)
		bw.Flush()
		flusher.Flush()
	}

	writeChunk(map[string]interface{}{"role": "assistant"}, nil)
	for _, tok := range tokens {
		writeChunk(map[string]interface{}{"content": tok}, nil)
		time.Sleep(15 * time.Millisecond)
	}
	writeChunk(map[string]interface{}{}, "stop")
	fmt.Fprint(bw, "data: [DONE]\n\n")
	bw.Flush()
	flusher.Flush()
}

func handleOpenAICompletion(w http.ResponseWriter, r *http.Request, nodeName, modelID string, latencyMs int) {
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream *bool  `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = modelID
	}

	tokens := openAITokensFor(req.Model)
	stream := req.Stream != nil && *req.Stream
	id := fmt.Sprintf("cmpl-mock-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	time.Sleep(time.Duration(latencyMs) * time.Millisecond)
	w.Header().Set("X-Node-Name", nodeName)

	if !stream {
		full := strings.Join(tokens, "")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": id, "object": "text_completion", "created": created, "model": req.Model,
			"choices": []map[string]interface{}{
				{"index": 0, "text": full, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 8, "completion_tokens": len(tokens), "total_tokens": 8 + len(tokens)},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	bw := bufio.NewWriter(w)
	for _, tok := range tokens {
		chunk := map[string]interface{}{
			"id": id, "object": "text_completion", "created": created, "model": req.Model,
			"choices": []map[string]interface{}{{"index": 0, "text": tok, "finish_reason": nil}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(bw, "data: %s\n\n", b)
		bw.Flush()
		flusher.Flush()
		time.Sleep(15 * time.Millisecond)
	}
	// Terminal chunk with finish_reason:"stop", matching the sibling
	// handleOpenAIChatCompletion above and the real OpenAI streaming
	// contract - this handler previously jumped straight from the last
	// per-token chunk (finish_reason:nil) to [DONE] with no stop signal.
	finalChunk := map[string]interface{}{
		"id": id, "object": "text_completion", "created": created, "model": req.Model,
		"choices": []map[string]interface{}{{"index": 0, "text": "", "finish_reason": "stop"}},
	}
	fb, _ := json.Marshal(finalChunk)
	fmt.Fprintf(bw, "data: %s\n\n", fb)
	fmt.Fprint(bw, "data: [DONE]\n\n")
	bw.Flush()
	flusher.Flush()
}
