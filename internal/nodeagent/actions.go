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
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
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
//
// Always builds an explicit env with HOME guaranteed present: a node agent
// running as a systemd service (or other stripped-down service environment)
// may have no $HOME set, and ollama's own CLI panics rather than falling
// back when it's missing - so the agent can't just trust its own inherited
// environment to be complete before handing it to a child process.
func runDownload(ctx context.Context, hfToken string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = ensureHome(os.Environ())
	if hfToken != "" {
		cmd.Env = append(cmd.Env, "HF_TOKEN="+hfToken)
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

// ensureHome returns env with a HOME entry guaranteed present, resolving it
// from the OS user database (not another read of $HOME) when missing so it
// doesn't just reproduce the same gap it's meant to fix.
func ensureHome(env []string) []string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") && kv != "HOME=" {
			return env
		}
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return env
	}
	return append(env, "HOME="+u.HomeDir)
}

func writeAction(w http.ResponseWriter, status int, resp actionResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// listModelsTimeout bounds how long the agent waits to enumerate local
// models. Deliberately short compared to pullTimeout - this is a read
// (an HTTP GET or a directory scan), never a transfer, so there is no
// legitimate reason for it to run long.
var listModelsTimeout = 30 * time.Second

// modelEntry is one entry in GET /v1/models' response - the Node Agent
// Protocol's "models" resource, capability "models.list". SizeBytes is
// omitted (never fabricated - R1) when the source can't report a real size.
type modelEntry struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	// Source records where this entry came from ("ollama-tags" or
	// "hf-cache") so a caller can tell "queried the runtime's own API" apart
	// from "scanned a cache directory the runtime reads from but doesn't
	// itself enumerate over HTTP" - the two have different freshness/
	// completeness guarantees.
	Source string `json:"source"`
}

type listModelsResponse struct {
	Models []modelEntry `json:"models"`
}

// listCommands maps a locally-detected runtime name to how this agent
// enumerates models already downloaded on this node (not just currently
// loaded - node.loadedModels, sourced from the mesh's own runtime probe,
// already covers that). Only Ollama exposes a real "everything downloaded"
// primitive over HTTP (GET /api/tags); tgi/vllm/llamacpp/mlx have no such
// endpoint - their own /v1/models (OpenAI-compat) reports only the
// currently-loaded model, not the full local cache - so they fall back to
// scanning the local Hugging Face cache directory, the same cache
// pullViaHFHub (above) downloads into.
var listCommands = map[string]func(ctx context.Context, runtimeURL string) ([]modelEntry, error){
	"ollama":   listViaOllamaTags,
	"tgi":      listViaHFCache,
	"vllm":     listViaHFCache,
	"llamacpp": listViaHFCache,
	"mlx":      listViaHFCache,
}

// handleListModels is the GET /v1/models handler, gated by the same
// per-node bearer token as every other route (see server.go/auth.go). A
// list is never a mutating action, so unlike handlePullModel this never uses
// actionResponse{ok,error} - success returns the model list directly, and
// failure returns a plain {"error": "..."} body.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	name, url := s.runtimeTarget()
	fn, ok := listCommands[name]
	if !ok {
		msg := fmt.Sprintf("unsupported: no model-listing primitive for runtime %q", name)
		if name == "" {
			msg = "unsupported: no inference runtime detected on this node"
		}
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), listModelsTimeout)
	defer cancel()
	models, err := fn(ctx, url)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listModelsResponse{Models: models})
}

// listViaOllamaTags queries Ollama's own GET /api/tags - the one runtime
// primitive in this fleet that genuinely reports "everything downloaded,"
// independent of what's currently loaded. Uses its own client (Timeout left
// zero, bounded purely by ctx) rather than Scheduler's runtimeClient - that
// client is hardcoded to a 5s timeout for the periodic warm-model probe,
// which would silently cap this read well under listModelsTimeout's
// intended 30s budget for a node with a large model catalog.
func listViaOllamaTags(ctx context.Context, runtimeURL string) ([]modelEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama /api/tags returned status %d", resp.StatusCode)
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	models := make([]modelEntry, 0, len(tags.Models))
	for _, m := range tags.Models {
		models = append(models, modelEntry{Name: m.Name, SizeBytes: m.Size, Source: "ollama-tags"})
	}
	return models, nil
}

// listViaHFCache scans the local Hugging Face cache directory for
// "models--<org>--<repo>" snapshot directories - the closest real equivalent
// to "everything downloaded" that tgi/vllm/llamacpp/mlx have, since none of
// them expose that over HTTP (see listCommands). runtimeURL is unused here
// (part of the shared listCommands signature) - this source is purely a
// filesystem read, never a network call.
func listViaHFCache(_ context.Context, _ string) ([]modelEntry, error) {
	dir, err := hfCacheDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []modelEntry{}, nil
		}
		return nil, err
	}
	models := make([]modelEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "models--") {
			continue
		}
		// A WalkDir error partway through (permission change, a file
		// deleted mid-scan) leaves size holding only a partial sum - never
		// report that as the real size (R1: an incomplete measurement is
		// not a measurement), so size_bytes is omitted instead.
		size, err := dirSize(filepath.Join(dir, e.Name()))
		if err != nil {
			size = 0
		}
		models = append(models, modelEntry{Name: hfCacheRepoID(e.Name()), SizeBytes: size, Source: "hf-cache"})
	}
	return models, nil
}

// hfCacheDir resolves the Hugging Face hub cache directory, honoring
// HF_HOME same as huggingface-cli itself, falling back to the OS user
// database (not another read of $HOME) when the environment doesn't have
// it set - same reasoning as ensureHome above (a service environment may
// have no $HOME at all).
func hfCacheDir() (string, error) {
	if v := os.Getenv("HF_HOME"); v != "" {
		return filepath.Join(v, "hub"), nil
	}
	home := os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	if home == "" {
		return "", errors.New("cannot resolve home directory to locate Hugging Face cache")
	}
	return filepath.Join(home, ".cache", "huggingface", "hub"), nil
}

// hfCacheRepoID converts a cache directory name ("models--org--repo") back
// to the "org/repo" Hugging Face identifier it was downloaded from.
func hfCacheRepoID(dirName string) string {
	parts := strings.SplitN(strings.TrimPrefix(dirName, "models--"), "--", 2)
	if len(parts) == 2 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// dirSize sums the real on-disk size of every regular file under path -
// never estimated (R1) - so a cached model's reported size reflects what's
// actually on the node's disk.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}
