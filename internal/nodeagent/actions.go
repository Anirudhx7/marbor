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

// deleteModelTimeout bounds how long the agent waits to remove a model.
// Short like listModelsTimeout, not generous like pullTimeout - a delete is
// a local rm/RemoveAll, never a network transfer, so there is no legitimate
// reason for it to run long even for a large model directory.
var deleteModelTimeout = 60 * time.Second

// deleteCommands maps a locally-detected runtime name to how this agent
// removes a downloaded model, mirroring pullCommands/listCommands. Ollama
// has a first-class delete primitive (`ollama rm`); tgi/vllm/llamacpp/mlx do
// not, so they fall back to removing the model's directory from the same
// local Hugging Face cache pullViaHFHub downloads into and listViaHFCache
// scans - the only place those runtimes' downloaded-but-not-loaded models
// actually live on disk.
var deleteCommands = map[string]func(ctx context.Context, model string) error{
	"ollama":   deleteViaOllama,
	"tgi":      deleteViaHFCache,
	"vllm":     deleteViaHFCache,
	"llamacpp": deleteViaHFCache,
	"mlx":      deleteViaHFCache,
}

// handleDeleteModel is the DELETE /v1/models/{name...} handler, gated by the
// same per-node bearer token as every other route. The model name is taken
// from the path rather than a request body - model names routinely contain
// "/" (e.g. "org/repo"), and Go's ServeMux "{name...}" wildcard captures the
// remaining path including any slashes, so there is no routing footgun to
// route around here the way a single-segment "{name}" would have.
func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("name")
	if model == "" {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "missing model name in path"})
		return
	}

	runtimeName, _ := s.runtimeTarget()
	fn, ok := deleteCommands[runtimeName]
	if !ok {
		msg := fmt.Sprintf("unsupported: no delete primitive for runtime %q", runtimeName)
		if runtimeName == "" {
			msg = "unsupported: no inference runtime detected on this node"
		}
		writeAction(w, http.StatusUnprocessableEntity, actionResponse{Error: msg})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), deleteModelTimeout)
	defer cancel()
	if err := fn(ctx, model); err != nil {
		writeAction(w, http.StatusBadGateway, actionResponse{Error: err.Error()})
		return
	}
	writeAction(w, http.StatusOK, actionResponse{OK: true})
}

// deleteViaOllama runs `ollama rm <model>` - same CLI-subprocess style as
// pullViaOllama, for the same reason: Ollama's own CLI is what already
// understands its model-name/tag format, so there is no benefit to
// reimplementing that against its HTTP DELETE /api/delete instead.
func deleteViaOllama(ctx context.Context, model string) error {
	return runDownload(ctx, "", "ollama", "rm", model)
}

// deleteViaHFCache removes a model's directory from the local Hugging Face
// cache - the fallback for runtimes with no first-class delete primitive of
// their own (see deleteCommands). model is attacker-influenced (it comes
// straight off the request path), so the directory name derived from it is
// resolved to an absolute path and checked to still live inside hfCacheDir()
// before anything is removed - a crafted model name must never be able to
// walk this destructive call outside the cache directory it's scoped to.
//
// Removing a model's files while a runtime process still has them open
// (mapped or loaded) is safe in practice: on Linux, unlink only detaches the
// directory entry - any process that already holds an open handle to a file
// underneath keeps working until it closes that handle on its own.
func deleteViaHFCache(_ context.Context, model string) error {
	dir, err := hfCacheDir()
	if err != nil {
		return err
	}
	cleanDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	dirName := "models--" + strings.ReplaceAll(hfRepoID(model), "/", "--")
	target, err := filepath.Abs(filepath.Join(cleanDir, dirName))
	if err != nil {
		return err
	}
	if target != cleanDir && !strings.HasPrefix(target, cleanDir+string(filepath.Separator)) {
		return fmt.Errorf("invalid model name %q", model)
	}

	// Lstat (not Stat) deliberately does not follow a symlink here - the
	// prefix check above only proves target's own path is inside cleanDir,
	// not what it resolves to. Without this, a symlink planted at that exact
	// path by any other process with write access to the cache dir would
	// pass the prefix check yet make RemoveAll recurse through the symlink
	// and delete whatever it points at outside the cache entirely.
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("model %q not found in local cache", model)
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid model name %q", model)
	}
	if !info.IsDir() {
		return fmt.Errorf("invalid model name %q", model)
	}

	return os.RemoveAll(target)
}

// unloadModelTimeout bounds how long the agent waits to evict a model from
// VRAM. Short like deleteModelTimeout - a local runtime call, never a
// network transfer, so there is no legitimate reason for it to run long.
var unloadModelTimeout = 30 * time.Second

// unloadCommands maps a locally-detected runtime name to how this agent
// evicts one model from VRAM while leaving the runtime process (and any
// other loaded models) running, mirroring pullCommands/listCommands/
// deleteCommands. Takes runtimeURL (unused by the CLI-subprocess entries,
// required by the HTTP one) for the same reason listCommands does - per
// .local/specs/node-agent-capabilities.md's verify-before-build note, only
// ollama and llamacpp get entries, deliberately, not an oversight:
//   - vLLM's sleep-mode (/sleep, /wake_up) is real but process-scoped (it
//     unloads *the* model in that process, not one of several) and gated
//     behind a dev-only env flag not safe to assume enabled.
//   - TGI is strictly single-model-per-process and the project is now
//     archived - unloading there is really "stop the process", a different
//     capability.
//   - The official mlx_lm.server has no multi-model or unload endpoint at
//     all; only third-party wrappers do.
//   - llama.cpp's router mode has a genuine per-model POST /models/unload
//     primitive (P32, following up on P31's deferral). The runtime-address
//     discovery P31 lacked already existed one layer up (RuntimeTarget's url,
//     the same one listCommands' HTTP entries use) - it just wasn't threaded
//     through unloadCommands yet. The remaining hazard: internal/runtime's
//     "llamacpp" signature match (GET /v1/models responding non-empty) is
//     true for both router mode and a plain single-model llama-server, which
//     has no /models* endpoints at all - so unloadViaLlamaCppRouter confirms
//     router mode is actually running (a side-effect-free GET {url}/models)
//     before ever attempting the unload, rather than assuming every node
//     identified as "llamacpp" supports it.
var unloadCommands = map[string]func(ctx context.Context, runtimeURL, model string) error{
	"ollama":   unloadViaOllama,
	"llamacpp": unloadViaLlamaCppRouter,
}

// handleUnloadModel is the POST /v1/models/{name...} handler, capability
// "models.unload", gated by the same per-node bearer token as every other
// route. model comes from the path, not a request body, matching
// handleDeleteModel's reasoning: model names routinely contain "/" (e.g.
// "org/repo"), so the trailing "{name...}" wildcard - not a literal "/unload"
// suffix - is what lets ServeMux capture the whole name including slashes.
// A trailing literal segment after a multi-segment wildcard isn't
// expressible in net/http's ServeMux, so POST (already unused on this path
// shape - pull owns the bare "POST /v1/models", delete owns
// "DELETE /v1/models/{name...}") is the verb that means "unload this model"
// here, the same way DELETE already means "delete this model" on the
// identical path shape.
func (s *Server) handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("name")
	if model == "" {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "missing model name in path"})
		return
	}

	runtimeName, runtimeURL := s.runtimeTarget()
	fn, ok := unloadCommands[runtimeName]
	if !ok {
		msg := fmt.Sprintf("unsupported: no unload primitive for runtime %q", runtimeName)
		if runtimeName == "" {
			msg = "unsupported: no inference runtime detected on this node"
		}
		writeAction(w, http.StatusUnprocessableEntity, actionResponse{Error: msg})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), unloadModelTimeout)
	defer cancel()
	if err := fn(ctx, runtimeURL, model); err != nil {
		writeAction(w, http.StatusBadGateway, actionResponse{Error: err.Error()})
		return
	}
	writeAction(w, http.StatusOK, actionResponse{OK: true})
}

// unloadViaOllama runs `ollama stop <model>` - Ollama's own CLI equivalent of
// the keep_alive:0 HTTP trick, evicting the model from VRAM while leaving the
// daemon and any other loaded models running. Same CLI-subprocess style as
// pullViaOllama/deleteViaOllama, for the same reason: Ollama's own CLI
// already understands its model-name/tag format. runtimeURL is unused here
// (part of unloadCommands' shared signature) - Ollama's CLI needs no address.
func unloadViaOllama(ctx context.Context, _, model string) error {
	return runDownload(ctx, "", "ollama", "stop", model)
}

// llamaCppRouterModelList is GET {runtimeURL}/models' response shape in
// llama.cpp router mode, confirmed against the official server README
// (tools/server/README.md, ggml-org/llama.cpp): {"data":[{"id":...,
// "status":{"value":"loaded"|"loading"|"unloaded",...}}]}. Only the fact
// that Data decodes as a non-nil array matters here - this type exists to
// confirm router mode is genuinely present, not to inspect any model's
// status.
type llamaCppRouterModelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// probeLlamaCppRouterMode issues a read-only GET {runtimeURL}/models - a
// route that only exists when llama-server is running in router mode (a
// plain single-model llama-server has no /models endpoint at all and
// answers 404). Distinct from the OpenAI-compatible GET /v1/models every
// llama-server build answers regardless of mode, which is what
// internal/runtime.DetectRuntime already uses to identify "llamacpp" in the
// first place - that signature alone cannot tell router mode from
// single-model mode, which is exactly why this second, side-effect-free
// probe exists before unloadViaLlamaCppRouter ever attempts a real unload.
func probeLlamaCppRouterMode(ctx context.Context, runtimeURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeURL+"/models", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /models returned status %d (not router mode)", resp.StatusCode)
	}
	var list llamaCppRouterModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return fmt.Errorf("GET /models response is not a router model list: %w", err)
	}
	if list.Data == nil {
		return fmt.Errorf("GET /models response missing \"data\" array (not router mode)")
	}
	return nil
}

// unloadViaLlamaCppRouter POSTs to llama.cpp router mode's own
// POST /models/unload {"model":"<name>"} (confirmed against
// tools/server/README.md) - a genuine per-model VRAM eviction that leaves
// the router process and every other loaded model running. Router mode is
// opt-in (llama-server enters it only when started without a model path,
// e.g. via --models-dir/--models-preset) and internal/runtime's "llamacpp"
// detection cannot distinguish it from a plain single-model instance, so
// this always confirms router mode via probeLlamaCppRouterMode first and
// refuses cleanly rather than guessing when it isn't there.
func unloadViaLlamaCppRouter(ctx context.Context, runtimeURL, model string) error {
	if err := probeLlamaCppRouterMode(ctx, runtimeURL); err != nil {
		return fmt.Errorf("unsupported: llama.cpp router mode not detected on this node (%w) - /models/unload only exists when llama-server runs in router mode", err)
	}

	body, err := json.Marshal(struct {
		Model string `json:"model"`
	}{Model: model})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, runtimeURL+"/models/unload", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama.cpp router /models/unload returned status %d", resp.StatusCode)
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("llama.cpp router /models/unload: could not decode response: %w", err)
	}
	if !out.Success {
		return fmt.Errorf("llama.cpp router /models/unload reported failure for model %q", model)
	}
	return nil
}
