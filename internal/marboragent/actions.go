package marboragent

// actions.go implements the Marbor Agent Protocol's first mutating resource
// (see .local/specs/node-agent.md section 16, node-agent-capabilities.md
// Group 2): POST /v1/models, capability "models.pull". The agent runs the
// locally-detected runtime's own model-download mechanism directly on the
// node, rather than marbor reaching the node's runtime HTTP API itself
// (admin.go's handleNodePull, the pre-existing path kept for nodes without
// an agent or an agent build predating this capability). This avoids two
// real problems with the old path: the marbor's own outbound HTTP client
// timeout being the wrong thing to bound a transfer it isn't a party to,
// and having no way to hand a Hugging Face token to the download at all.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
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
	// Driver/Identifier mirror controlActionRequest's fields (control_actions.go)
	// - marbor constructs them fresh from its own store-backed
	// router.ControlConfig cache on every request, same as it does for
	// runtime.start/stop/restart. Empty when the node has no control driver
	// configured (the common systemd/native-process case), in which case
	// runDownload behaves exactly as before this field existed. Only
	// Driver=="docker" changes anything: the runtime's own CLI (ollama,
	// huggingface-cli, ...) lives inside the container, not this agent
	// process's host PATH, so the download command must run via `docker exec
	// <Identifier> ...` instead of directly.
	Driver     string `json:"driver,omitempty"`
	Identifier string `json:"identifier,omitempty"`
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
var pullCommands = map[string]func(ctx context.Context, model, hfToken, driver, identifier string) error{
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req pullModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "missing or invalid model field"})
		return
	}
	if strings.HasPrefix(req.Model, "-") {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "invalid model field"})
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
	if err := fn(ctx, req.Model, req.HFToken, req.Driver, req.Identifier); err != nil {
		writeAction(w, http.StatusBadGateway, actionResponse{Error: err.Error()})
		return
	}
	writeAction(w, http.StatusOK, actionResponse{OK: true})
}

// pullViaOllama runs `ollama pull <model>` - model is passed through exactly
// as given (e.g. "hf.co/org/repo:Q4_K_M" or an official library tag like
// "llama3:8b"), since Ollama's own CLI is what resolves that tag format.
func pullViaOllama(ctx context.Context, model, hfToken, driver, identifier string) error {
	return runDownload(ctx, driver, identifier, hfToken, "ollama", "pull", "--", model)
}

// pullViaTGI runs `text-generation-server download-weights <repo>`. TGI's
// download step takes a bare Hugging Face repo id, not an Ollama-style
// "hf.co/...:quant" tag (TGI serves full-precision/HF-format weights, not
// GGUF quant files) - hfRepoID strips the parts that are meaningless here.
// The lookPath preflight is skipped for the docker driver: it checks this
// agent process's own host PATH, which says nothing about what's installed
// inside the container runDownload will actually exec into.
func pullViaTGI(ctx context.Context, model, hfToken, driver, identifier string) error {
	if driver != "docker" {
		if _, err := lookPath("text-generation-server"); err != nil {
			return errors.New("unsupported: text-generation-server not found on PATH")
		}
	}
	return runDownload(ctx, driver, identifier, hfToken, "text-generation-server", "download-weights", "--", hfRepoID(model))
}

// pullViaHFHub is the fallback for runtimes (vLLM, llama.cpp) with no
// first-class standalone pull command of their own: `huggingface-cli
// download <repo>` populates the shared local HF cache both runtimes read
// from when a model is loaded. Requires huggingface-cli (part of the
// huggingface_hub package) to already be present on the node - both vLLM
// and TGI depend on huggingface_hub internally, so it is very likely
// already installed alongside either; a node genuinely missing it gets a
// clear, honest error rather than a silent no-op (R1 extended to actions:
// an action that didn't happen must never report ok:true). Same
// docker-driver lookPath skip as pullViaTGI, for the same reason.
func pullViaHFHub(ctx context.Context, model, hfToken, driver, identifier string) error {
	if driver != "docker" {
		if _, err := lookPath("huggingface-cli"); err != nil {
			return errors.New("unsupported: huggingface-cli not found on PATH (required to pull models for this runtime)")
		}
	}
	return runDownload(ctx, driver, identifier, hfToken, "huggingface-cli", "download", "--", hfRepoID(model))
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
// When driver is "docker" and identifier is non-empty, name+args are run
// inside that container via `docker exec` instead of directly on this host -
// the runtime's own CLI (ollama, huggingface-cli, ...) lives in the
// container's filesystem/PATH, not this agent process's, when the runtime is
// deployed that way (P43's ControlDriver abstraction already knows this for
// start/stop/restart/logs; pull/delete/unload need the same awareness).
// HF_TOKEN is passed via `docker exec -e` in that case, since a container's
// exec environment is set per-invocation, not by mutating a *exec.Cmd.Env
// that only affects the host-side `docker` process itself.
//
// Otherwise (native/systemd/process driver, or no driver configured at all -
// the pre-existing behavior for every node before this field existed) always
// builds an explicit env with HOME guaranteed present: a Marbor agent running
// as a systemd service (or other stripped-down service environment) may have
// no $HOME set, and ollama's own CLI panics rather than falling back when
// it's missing - so the agent can't just trust its own inherited environment
// to be complete before handing it to a child process.
func runDownload(ctx context.Context, driver, identifier, hfToken string, name string, args ...string) error {
	var cmd *exec.Cmd
	if driver == "docker" && identifier != "" {
		dockerArgs := []string{"exec"}
		if hfToken != "" {
			dockerArgs = append(dockerArgs, "-e", "HF_TOKEN="+hfToken)
		}
		dockerArgs = append(dockerArgs, identifier, name)
		dockerArgs = append(dockerArgs, args...)
		cmd = exec.CommandContext(ctx, "docker", dockerArgs...)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
		cmd.Env = ensureHome(os.Environ())
		if hfToken != "" {
			cmd.Env = append(cmd.Env, "HF_TOKEN="+hfToken)
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := lastMeaningfulLine(stripANSI(stderr.String()))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

// lastMeaningfulLine returns the last non-empty, trimmed line of s. A CLI
// not attached to a real TTY (as every one of these subprocesses is) writes
// a fresh stderr line per progress tick instead of overwriting the same
// line in place via cursor-movement escapes - stripANSI removes the escape
// codes but not the hundreds of near-identical "pulling <digest>: N%" lines
// they were meant to collapse into one. Returning that entire multi-hundred-
// line transcript as "the error" is what blows up the admin UI's pull toast
// into an unreadable, viewport-covering wall of repeated text; the actual
// failure reason is always the last line the process wrote before exiting
// non-zero (true for ollama's own CLI, and for huggingface-cli/Python
// tracebacks, which likewise put the real exception message last).
func lastMeaningfulLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// ansiEscapeSequence matches CSI (Control Sequence Introducer) escapes -
// \x1b[ followed by any private-mode marker/parameter bytes and a final
// letter, e.g. \x1b[?25l (hide cursor), \x1b[?2026h (synchronized-update
// mode), \x1b[1G (cursor to column), \x1b[K (erase line). The `ollama` CLI
// draws a terminal spinner using exactly these on stderr even when its
// output is captured rather than attached to a real TTY - uncleaned, they
// show up as garbled box characters in any error message surfaced to the
// admin UI (R1: real error text, not real-but-unreadable error text).
var ansiEscapeSequence = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansiEscapeSequence.ReplaceAllString(s, "")
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

// modelEntry is one entry in GET /v1/models' response - the Marbor Agent
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
	// Family is Ollama's own architecture classification (e.g. "llama",
	// "bert"), letting a caller distinguish chat-capable models from
	// embedding/encoder-only ones. Omitted (never guessed - R1) for sources
	// that can't report it - today only listViaOllamaTags populates this;
	// listViaHFCache's directory scan has no such metadata available.
	Family string `json:"family,omitempty"`
}

type listModelsResponse struct {
	Models []modelEntry `json:"models"`
}

// listCommands maps a locally-detected runtime name to how this agent
// enumerates models already downloaded on this node (not just currently
// loaded - node.loadedModels, sourced from the marbor's own runtime probe,
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
			Name    string `json:"name"`
			Size    int64  `json:"size"`
			Details struct {
				Family string `json:"family"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	models := make([]modelEntry, 0, len(tags.Models))
	for _, m := range tags.Models {
		models = append(models, modelEntry{Name: m.Name, SizeBytes: m.Size, Source: "ollama-tags", Family: m.Details.Family})
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
var deleteCommands = map[string]func(ctx context.Context, model, driver, identifier string) error{
	"ollama":   deleteViaOllama,
	"tgi":      deleteViaHFCache,
	"vllm":     deleteViaHFCache,
	"llamacpp": deleteViaHFCache,
	"mlx":      deleteViaHFCache,
}

// controlRequest carries the same Driver/Identifier fields as
// pullModelRequest (actions.go)/controlActionRequest (control_actions.go) -
// the marbor's per-request {driver, identifier} injection, threaded through
// delete/unload the same way pull already is. A DELETE/POST with no body (or
// an empty one) decodes to the zero value, which is exactly the pre-existing
// behavior (native/no-driver-configured), so this is additive-only for every
// node that isn't Docker-controlled.
type controlRequest struct {
	Driver     string `json:"driver,omitempty"`
	Identifier string `json:"identifier,omitempty"`
}

// decodeControlRequest reads an optional JSON body for Driver/Identifier,
// tolerating a missing/empty body (io.EOF) since neither delete nor unload
// required a request body before this field existed - only a genuinely
// malformed non-empty body is an error.
func decodeControlRequest(w http.ResponseWriter, r *http.Request) (controlRequest, error) {
	var req controlRequest
	if r.Body == nil {
		return req, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		return controlRequest{}, err
	}
	return req, nil
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
	if strings.HasPrefix(model, "-") {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "invalid model name"})
		return
	}
	ctrl, err := decodeControlRequest(w, r)
	if err != nil {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "invalid request body"})
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
	if err := fn(ctx, model, ctrl.Driver, ctrl.Identifier); err != nil {
		writeAction(w, http.StatusBadGateway, actionResponse{Error: err.Error()})
		return
	}
	writeAction(w, http.StatusOK, actionResponse{OK: true})
}

// deleteViaOllama runs `ollama rm <model>` - same CLI-subprocess style as
// pullViaOllama, for the same reason: Ollama's own CLI is what already
// understands its model-name/tag format, so there is no benefit to
// reimplementing that against its HTTP DELETE /api/delete instead.
func deleteViaOllama(ctx context.Context, model, driver, identifier string) error {
	return runDownload(ctx, driver, identifier, "", "ollama", "rm", "--", model)
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
//
// driver/identifier are accepted (matching deleteCommands' shared signature)
// but unused - this scans the agent process's own host filesystem cache,
// which is a separate, pre-existing gap from the one this change fixes: a
// containerized vLLM/TGI/llama.cpp/mlx's actual HF cache lives inside that
// container's filesystem, not this host's, so this path is only correct for
// a natively-installed runtime regardless of driver. Tracked as a distinct
// follow-up, not folded into this fix (different mechanism - a directory
// scan, not a subprocess exec - so `docker exec` doesn't apply the same way).
func deleteViaHFCache(_ context.Context, model, _, _ string) error {
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
var unloadCommands = map[string]func(ctx context.Context, runtimeURL, model, driver, identifier string) error{
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
	if strings.HasPrefix(model, "-") {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "invalid model name"})
		return
	}
	ctrl, err := decodeControlRequest(w, r)
	if err != nil {
		writeAction(w, http.StatusBadRequest, actionResponse{Error: "invalid request body"})
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
	if err := fn(ctx, runtimeURL, model, ctrl.Driver, ctrl.Identifier); err != nil {
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
func unloadViaOllama(ctx context.Context, _, model, driver, identifier string) error {
	return runDownload(ctx, driver, identifier, "", "ollama", "stop", "--", model)
}

// llamaCppRouterModelList is GET {runtimeURL}/models' response shape in
// llama.cpp router mode, confirmed against a real router-mode instance
// (ghcr.io/ggml-org/llama.cpp:server, 2026-07-28, HF-cache-backed
// --models-dir): {"data":[{"id":...,"status":{"value":"loaded"|"loading"|
// "unloaded","args":["/app/llama-server",...,"--model","<full path>"]}}]}.
// Args is what resolveLlamaCppRouterModelID matches against - the router's
// own "id" is a bare filename stem (e.g. "Qwen2.5-0.5B-Instruct-Q4_K_M"),
// never the Hugging Face "org/repo" every other marbor code path (hfRepoID,
// hfCacheRepoID) uses to name an HF-cache-sourced model, so an org/repo
// unload request can never exact-match "id" directly - see
// resolveLlamaCppRouterModelID's doc comment for the resulting fix.
type llamaCppRouterModelList struct {
	Data []struct {
		ID     string `json:"id"`
		Status struct {
			Args []string `json:"args"`
		} `json:"status"`
	} `json:"data"`
}

// fetchLlamaCppRouterModels issues a read-only GET {runtimeURL}/models - a
// route that only exists when llama-server is running in router mode (a
// plain single-model llama-server has no /models endpoint at all and
// answers 404). Distinct from the OpenAI-compatible GET /v1/models every
// llama-server build answers regardless of mode, which is what
// internal/runtime.DetectRuntime already uses to identify "llamacpp" in the
// first place - that signature alone cannot tell router mode from
// single-model mode, which is exactly why this second, side-effect-free
// call exists before unloadViaLlamaCppRouter ever attempts a real unload.
// Serves double duty as the router-mode probe (a non-router server 404s or
// returns no "data" array) and as the source list resolveLlamaCppRouterModelID
// matches model names against, avoiding a second GET for the same data.
func fetchLlamaCppRouterModels(ctx context.Context, runtimeURL string) (*llamaCppRouterModelList, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /models returned status %d (not router mode)", resp.StatusCode)
	}
	var list llamaCppRouterModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("GET /models response is not a router model list: %w", err)
	}
	if list.Data == nil {
		return nil, fmt.Errorf("GET /models response missing \"data\" array (not router mode)")
	}
	return &list, nil
}

// resolveLlamaCppRouterModelID maps model (as sent by marbor callers - either
// an already-exact router id, or the "org/repo" Hugging Face identifier
// hfRepoID/hfCacheRepoID use for every other HF-cache-sourced model name in
// this codebase) to the exact router "id" POST /models/unload requires.
//
// Confirmed 2026-07-28 against a real router-mode instance: the router's
// "id" is a bare filename stem ("Qwen2.5-0.5B-Instruct-Q4_K_M"), which has
// no substring relationship to "org/repo" at all - the P34 queue item's
// original assumption that "id" looked like "org/repo:QUANT" was verified
// INVALID. The only place "org/repo" survives is inside each entry's
// status.args, in the "--model" flag's value - the on-disk HF cache path
// (".../models--org--repo/snapshots/<hash>/<file>.gguf") pullViaHFHub
// downloads into and listViaHFCache/deleteViaHFCache already key off of via
// the same "models--org--repo" directory-name convention.
//
// A repo with multiple quant files (the common case for GGUF repos - this
// exact test fixture has two) has multiple router ids whose --model path
// all fall under the same "models--org--repo" directory. That makes
// "org/repo" alone genuinely ambiguous among which quant to unload - this
// deliberately refuses to guess (matching R1's "honest error over a false
// success" and the queue's "single match = use it, zero or multiple = clear
// error, never guess" design constraint) and instead reports the candidate
// ids so the caller can retry with one.
func resolveLlamaCppRouterModelID(list *llamaCppRouterModelList, model string) (string, error) {
	for _, entry := range list.Data {
		if entry.ID == model {
			return entry.ID, nil
		}
	}

	if !strings.Contains(model, "/") {
		return "", fmt.Errorf("model %q not found (no loaded router id matches)", model)
	}
	dirName := "models--" + strings.ReplaceAll(hfRepoID(model), "/", "--")
	needle := "/" + dirName + "/"

	var matches []string
	for _, entry := range list.Data {
		for i, arg := range entry.Status.Args {
			if arg != "--model" || i+1 >= len(entry.Status.Args) {
				continue
			}
			if strings.Contains(entry.Status.Args[i+1], needle) {
				matches = append(matches, entry.ID)
			}
			break
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("model %q not found (no loaded router preset's path matches this repo)", model)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("model %q is ambiguous - %d router presets match this repo (%s); retry the unload with one of these exact ids instead of the repo name", model, len(matches), strings.Join(matches, ", "))
	}
}

// unloadViaLlamaCppRouter POSTs to llama.cpp router mode's own
// POST /models/unload {"model":"<name>"} (confirmed against
// tools/server/README.md) - a genuine per-model VRAM eviction that leaves
// the router process and every other loaded model running. Router mode is
// opt-in (llama-server enters it only when started without a model path,
// e.g. via --models-dir/--models-preset) and internal/runtime's "llamacpp"
// detection cannot distinguish it from a plain single-model instance, so
// this always confirms router mode via fetchLlamaCppRouterModels first and
// refuses cleanly rather than guessing when it isn't there. model is then
// resolved from marbor's own naming (org/repo, or an already-exact id) to the
// router's own id via resolveLlamaCppRouterModelID before the real POST -
// see that function's doc comment for why "org/repo" alone cannot be sent
// to the router directly (P34).
func unloadViaLlamaCppRouter(ctx context.Context, runtimeURL, model, _, _ string) error {
	list, err := fetchLlamaCppRouterModels(ctx, runtimeURL)
	if err != nil {
		return fmt.Errorf("unsupported: llama.cpp router mode not detected on this node (%w) - /models/unload only exists when llama-server runs in router mode", err)
	}
	resolvedID, err := resolveLlamaCppRouterModelID(list, model)
	if err != nil {
		return err
	}

	body, err := json.Marshal(struct {
		Model string `json:"model"`
	}{Model: resolvedID})
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

// healthCheckTimeout bounds how long the agent waits for an on-demand
// liveness probe. Short, like listModelsTimeout/deleteModelTimeout - an
// operator hitting "check now" wants a fast, fresh answer, not something
// that can hang for as long as a model transfer.
var healthCheckTimeout = 10 * time.Second

// healthCheckResult is GET /v1/runtime/health's response body, capability
// "runtime.health_check". LatencyMs is a real time.Since measurement around
// the probe call, never estimated (R1). No omitempty - a genuinely fast
// (0ms) probe is a real measurement, not an absent one, and omitempty on an
// int64 would silently drop it, indistinguishable from "not reported."
// LatencyMs is meaningless when OK is false (the probe never completed) and
// left at its zero value in that case - callers must check OK first.
type healthCheckResult struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
}

// healthCheckCommands maps a locally-detected runtime name to this agent's
// on-demand liveness probe, mirroring pullCommands/listCommands/
// deleteCommands/unloadCommands. Unlike unloadCommands, every runtime has a
// genuine liveness signal - none of the "no primitive exists" gaps documented
// above unloadCommands apply here:
//   - vLLM/TGI/llama.cpp all expose a real GET /health (llama.cpp's answers
//     in both plain and router mode, unlike its router-only GET /models).
//   - Ollama has no separate /health route, but GET /api/tags (already used
//     by listViaOllamaTags) is itself a genuine liveness signal - a lighter
//     call than GET /api/ps, and it errors the same way on an unreachable
//     daemon.
//   - mlx_lm.server's official server exposes no /health route at all; a
//     successful GET /v1/models response IS the reachability signal, the
//     same reasoning internal/runtime's mlx probe already documents.
var healthCheckCommands = map[string]func(ctx context.Context, runtimeURL string) error{
	"ollama":   healthCheckViaOllamaTags,
	"vllm":     healthCheckViaHTTPGet("/health"),
	"tgi":      healthCheckViaHTTPGet("/health"),
	"llamacpp": healthCheckViaHTTPGet("/health"),
	"mlx":      healthCheckViaHTTPGet("/v1/models"),
}

// handleHealthCheck is the GET /v1/runtime/health handler, capability
// "runtime.health_check", gated by the same per-node bearer token as every
// other route. Always returns 200 with a real ok/error/latency_ms result -
// even a failed probe is a successful health check (the answer is "down"),
// so unlike the mutating actions this never uses the 4xx/5xx status to carry
// the failure; the body's ok field does that, matching the Group 2 wire
// doc's { "ok": true } / { "ok": false, "error": "..." } shape.
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	name, url := s.runtimeTarget()
	fn, ok := healthCheckCommands[name]
	if !ok {
		msg := fmt.Sprintf("unsupported: no health-check primitive for runtime %q", name)
		if name == "" {
			msg = "unsupported: no inference runtime detected on this node"
		}
		writeAction(w, http.StatusUnprocessableEntity, actionResponse{Error: msg})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	start := time.Now()
	err := fn(ctx, url)
	latency := time.Since(start)

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(healthCheckResult{OK: false, Error: err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(healthCheckResult{OK: true, LatencyMs: latency.Milliseconds()})
}

// healthCheckViaOllamaTags probes Ollama's own GET /api/tags - the same
// primitive listViaOllamaTags uses to enumerate models, and a genuine
// liveness signal: it only succeeds if the daemon is up and answering.
func healthCheckViaOllamaTags(ctx context.Context, runtimeURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama /api/tags returned status %d", resp.StatusCode)
	}
	return nil
}

// healthCheckViaHTTPGet returns a healthCheckCommands entry that probes a
// fixed path on runtimeURL and treats any 200 response as healthy - shared by
// every runtime whose liveness signal is simply "this endpoint answers"
// (vLLM/TGI/llama.cpp's real /health, mlx's /v1/models standing in for one).
func healthCheckViaHTTPGet(path string) func(ctx context.Context, runtimeURL string) error {
	return func(ctx context.Context, runtimeURL string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeURL+path, nil)
		if err != nil {
			return err
		}
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s returned status %d", path, resp.StatusCode)
		}
		return nil
	}
}
