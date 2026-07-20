package nodeagent

// actions.go implements the Node Agent Protocol's first mutating resource
// (see .local/specs/node-agent.md section 16, node-agent-capabilities.md
// Group 2): POST /v1/models, capability "models.pull". The agent runs the
// locally-detected runtime's own model-download mechanism directly on the
// node, rather than the mesh reaching the node's runtime HTTP API itself
// (admin.go's handleNodePull, the pre-existing path kept for nodes without
// an agent or an agent build predating this capability). This avoids two
// real problems with the old path: the mesh's own outbound HTTP client
// timeout being the wrong thing to bound a transfer it isn't a party to,
// and having no way to hand a Hugging Face token to the download at all.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// pullTimeout bounds how long the agent waits for a model download to
// finish locally. Generous for the same reason admin.go's nodePullTimeout
// is: large/gated Hugging Face GGUF downloads legitimately take a long
// time, and killing one mid-download to enforce a short timeout would
// manufacture a failure out of a transfer that was otherwise succeeding.
var pullTimeout = 2 * time.Hour

type pullModelRequest struct {
	Model string `json:"model"`
	// HFToken is per-request only - the agent never persists it to disk and
	// never logs it. It is set in the download subprocess's own environment
	// for the lifetime of that one command, never the agent process's own
	// environment.
	HFToken string `json:"hf_token,omitempty"`
}

type actionResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// pullCommands maps a locally-detected runtime name (Scheduler.Runtime) to
// the mechanism this agent uses to fetch a model for it. Adding a vendor
// means adding one entry here - nothing else in this file changes. Runtimes
// without a first-class standalone "pull without loading" primitive (vLLM,
// llama.cpp, mlx) fall back to `huggingface-cli download`, which populates
// the same local Hugging Face cache those runtimes read models from at load
// time - the closest real equivalent of "pull" they have. mlx-lm models are
// downloaded the same way (safetensors + config.json in MLX's own quant
// format, tagged library:mlx on HF) - mlx-lm itself has no standalone pull
// command either.
var pullCommands = map[string]func(ctx context.Context, model, hfToken string) error{
	"ollama":   pullViaOllama,
	"tgi":      pullViaTGI,
	"vllm":     pullViaHFHub,
	"llamacpp": pullViaHFHub,
	"mlx":      pullViaHFHub,
}

// handlePullModel is the POST /v1/models handler, gated by the same
// per-node bearer token as /v1/status and /metrics (see server.go/auth.go -
// no new auth mechanism for this action).
func (s *Server) handlePullModel(w http.ResponseWriter, r *http.Request) {
	var req pullModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "missing or invalid model field"})
		return
	}

	var runtimeName string
	if rt := s.snapshot().Runtime; rt != nil {
		runtimeName = rt.Name
	}
	fn, ok := pullCommands[runtimeName]
	if !ok {
		msg := fmt.Sprintf("unsupported: no pull primitive for runtime %q", runtimeName)
		if runtimeName == "" {
			msg = "unsupported: no inference runtime detected on this node"
		}
		writeAction(w, http.StatusUnprocessableEntity, actionResponse{Error: msg})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), pullTimeout)
	defer cancel()
	if err := fn(ctx, req.Model, req.HFToken); err != nil {
		writeAction(w, http.StatusBadGateway, actionResponse{Error: err.Error()})
		return
	}
	writeAction(w, http.StatusOK, actionResponse{OK: true})
}

// pullViaOllama runs `ollama pull <model>` - model is passed through exactly
// as given (e.g. "hf.co/org/repo:Q4_K_M" or an official library tag like
// "llama3:8b"), since Ollama's own CLI is what resolves that tag format.
func pullViaOllama(ctx context.Context, model, hfToken string) error {
	return runDownload(ctx, hfToken, "ollama", "pull", model)
}

// pullViaTGI runs `text-generation-server download-weights <repo>`. TGI's
// download step takes a bare Hugging Face repo id, not an Ollama-style
// "hf.co/...:quant" tag (TGI serves full-precision/HF-format weights, not
// GGUF quant files) - hfRepoID strips the parts that are meaningless here.
func pullViaTGI(ctx context.Context, model, hfToken string) error {
	if _, err := lookPath("text-generation-server"); err != nil {
		return errors.New("unsupported: text-generation-server not found on PATH")
	}
	return runDownload(ctx, hfToken, "text-generation-server", "download-weights", hfRepoID(model))
}

// pullViaHFHub is the fallback for runtimes (vLLM, llama.cpp) with no
// first-class standalone pull command of their own: `huggingface-cli
// download <repo>` populates the shared local HF cache both runtimes read
// from when a model is loaded. Requires huggingface-cli (part of the
// huggingface_hub package) to already be present on the node - both vLLM
// and TGI depend on huggingface_hub internally, so it is very likely
// already installed alongside either; a node genuinely missing it gets a
// clear, honest error rather than a silent no-op (R1 extended to actions:
// an action that didn't happen must never report ok:true).
func pullViaHFHub(ctx context.Context, model, hfToken string) error {
	if _, err := lookPath("huggingface-cli"); err != nil {
		return errors.New("unsupported: huggingface-cli not found on PATH (required to pull models for this runtime)")
	}
	return runDownload(ctx, hfToken, "huggingface-cli", "download", hfRepoID(model))
}

// hfRepoID strips an Ollama-style "hf.co/" prefix and ":quant" suffix down
// to the bare "org/repo" Hugging Face identifier other tools expect. Only
// strips a trailing ":..." when what precedes it still looks like an
// "org/repo" pair, so a plain runtime-native tag without a Hugging Face
// shape is never mangled.
func hfRepoID(model string) string {
	repo := strings.TrimPrefix(model, "hf.co/")
	if idx := strings.LastIndex(repo, ":"); idx != -1 && strings.Contains(repo[:idx], "/") {
		repo = repo[:idx]
	}
	return repo
}

// runDownload executes name+args with hfToken (if any) set as HF_TOKEN in
// the child process's own environment only - never the agent process's
// environment, never written to disk, never logged. Returns the command's
// stderr (trimmed) as the error on failure, since that is almost always the
// actual reason (gated repo, 404, disk full, etc.) an operator needs to see
// - never a bare "exit status 1".
func runDownload(ctx context.Context, hfToken string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if hfToken != "" {
		cmd.Env = append(os.Environ(), "HF_TOKEN="+hfToken)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

func writeAction(w http.ResponseWriter, status int, resp actionResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
