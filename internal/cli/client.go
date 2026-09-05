// Package cli implements the marbor CLI - a thin client of the Admin API.
// The CLI never talks to a Marbor Agent directly; every command is exactly
// one Admin API request.
package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Exit codes: 0 ok, 1 user error, 2 server error, 4 auth error.
// 3 (partial success) doesn't apply until batch operations exist.
const (
	ExitOK          = 0
	ExitUserError   = 1
	ExitServerError = 2
	ExitAuthError   = 4
)

// CLIError carries the exit code a failure should produce, so cli.go's
// dispatcher is the only place that translates errors into os.Exit calls.
type CLIError struct {
	Code int
	Msg  string
}

func (e *CLIError) Error() string { return e.Msg }

func userErrorf(format string, args ...interface{}) error {
	return &CLIError{Code: ExitUserError, Msg: fmt.Sprintf(format, args...)}
}

func serverErrorf(format string, args ...interface{}) error {
	return &CLIError{Code: ExitServerError, Msg: fmt.Sprintf(format, args...)}
}

func authErrorf(format string, args ...interface{}) error {
	return &CLIError{Code: ExitAuthError, Msg: fmt.Sprintf(format, args...)}
}

// Client is a thin wrapper over the Admin API's HTTP surface.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client

	// Username, Role, and ExpiresAt are populated by Login from the
	// /admin/v1/login response body - empty when the client was instead
	// built from a --token flag/env or a saved session (neither of those
	// paths calls Login, and the Admin API has no session-introspection
	// endpoint today to recover identity from a bare token - see the
	// runWhoami doc comment in auth_cmds.go).
	Username  string
	Role      string
	ExpiresAt string

	// usingSavedSession is set by authenticatedClient when Token came from
	// the persisted session file (internal/cli/session.go), not a --token
	// flag/env or a fresh --username/--password login. doRequest and
	// doRequestBody use it to append a "run marbor login again" hint
	// to a 401/403 - the exact "clear message" the CLI persistent-auth
	// queue item calls for, produced from a real server response rather
	// than a local expiry guess (the saved session intentionally carries no
	// expiry timestamp - the server is the sole source of truth for that).
	usingSavedSession bool
}

// NewClient builds a Client for baseURL. token may be empty; Login can be
// used to obtain one.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		Token:      token,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Login calls POST /admin/v1/login with username/password and captures the
// session token from the Set-Cookie response (the token is never echoed in
// the JSON body - admin.go:handleLoginForRole only sets it via cookie).
// On success it also stores the token on the Client for subsequent calls.
func (c *Client) Login(username, password string) error {
	payload, err := json.Marshal(struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{username, password})
	if err != nil {
		return userErrorf("building login request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/admin/v1/login", bytes.NewReader(payload))
	if err != nil {
		return userErrorf("building login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return serverErrorf("could not reach %s: %v", c.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return authErrorf("login failed: %s", readErrorMessage(resp.Body))
	}
	if resp.StatusCode != http.StatusOK {
		return serverErrorf("login failed with status %d: %s", resp.StatusCode, readErrorMessage(resp.Body))
	}

	var respBody struct {
		Username           string `json:"username"`
		Role               string `json:"role"`
		ExpiresAt          string `json:"expires_at"`
		MustChangePassword bool   `json:"must_change_password"`
	}
	// Best-effort: a malformed body here doesn't invalidate a successful
	// login (the cookie is still authoritative), so decode errors are not
	// fatal - just skip the must-change-password fast-path and identity
	// fields below.
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(bodyBytes, &respBody)

	for _, cookie := range resp.Cookies() {
		if cookie.Name == "marbor_session" {
			c.Token = cookie.Value
			c.Username = respBody.Username
			c.Role = respBody.Role
			c.ExpiresAt = respBody.ExpiresAt
			if respBody.MustChangePassword {
				return authErrorf("password change required: log in via the web dashboard once to set a new password before using the CLI")
			}
			return nil
		}
	}
	return serverErrorf("login succeeded but no session cookie was returned")
}

// doRequest performs an HTTP request against the Admin API, attaching the
// bearer token when authed is true. It classifies the response into the
// CLI's exit-code taxonomy.
func (c *Client) doRequest(method, path string, authed bool) (*http.Response, error) {
	req, err := http.NewRequest(method, c.BaseURL+path, nil)
	if err != nil {
		return nil, userErrorf("building request: %v", err)
	}
	if authed {
		if c.Token == "" {
			return nil, userErrorf("authentication required: run marbor login, or pass --username/--password (or MARBOR_USERNAME+MARBOR_PASSWORD)")
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, serverErrorf("could not reach %s: %v", c.BaseURL, err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		defer resp.Body.Close()
		return nil, authErrorf("%s%s", readErrorMessage(resp.Body), c.savedSessionHint())
	case resp.StatusCode == http.StatusServiceUnavailable:
		// GET /health returns 503 with a still-decodable body to signal a
		// degraded (not down) marbor, not a hard failure - let the caller
		// decode it instead of discarding it as a generic server error.
		return resp, nil
	case resp.StatusCode >= 500:
		defer resp.Body.Close()
		return nil, serverErrorf("server error (%d): %s", resp.StatusCode, readErrorMessage(resp.Body))
	case resp.StatusCode >= 400:
		defer resp.Body.Close()
		return nil, serverErrorf("unexpected response (%d): %s", resp.StatusCode, readErrorMessage(resp.Body))
	}
	return resp, nil
}

// doRequestBody performs an authed HTTP request with a JSON body against
// the Admin API, classified into the same exit-code taxonomy as doRequest.
// Every mutating CLI command (runtime start/stop/restart, node control
// accept) goes through this - the CLI is always exactly one Admin API
// request, never a direct Marbor Agent call.
func (c *Client) doRequestBody(method, path string, body interface{}) (*http.Response, error) {
	if c.Token == "" {
		return nil, userErrorf("authentication required: run 'marbor login', or pass --username/--password (or set MARBOR_USERNAME+MARBOR_PASSWORD)")
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, userErrorf("building request body: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, reader)
	if err != nil {
		return nil, userErrorf("building request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, serverErrorf("could not reach %s: %v", c.BaseURL, err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		defer resp.Body.Close()
		return nil, authErrorf("%s%s", readErrorMessage(resp.Body), c.savedSessionHint())
	case resp.StatusCode == http.StatusUnprocessableEntity:
		// 422 (e.g. "no control driver configured") is a user error, not a
		// server error - the request reached the server fine, the operator
		// just needs to configure something first.
		defer resp.Body.Close()
		return nil, userErrorf("%s", readErrorMessage(resp.Body))
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented:
		defer resp.Body.Close()
		return nil, userErrorf("%s", readErrorMessage(resp.Body))
	case resp.StatusCode >= 400:
		defer resp.Body.Close()
		return nil, serverErrorf("unexpected response (%d): %s", resp.StatusCode, readErrorMessage(resp.Body))
	}
	return resp, nil
}

// Logout calls POST /logout to end the current session server-side. It uses
// the role-agnostic "/logout" route (sessionAuth, any valid session) rather
// than the admin-only "/admin/logout", since a CLI session may belong to any
// authenticated role, not just admin - "/admin/v1/logout" does not exist as
// a registered route (only "/admin/logout" and "/logout" do; see admin.go's
// mux registration), so this is the closest real endpoint that identifies
// and invalidates the bearer token's session the way logout needs.
// Deliberately just another doRequestBody call, classified the same as
// every other mutating call - the caller (runLogout) is what treats any
// error here as a soft, non-fatal warning, not this method.
func (c *Client) Logout() error {
	resp, err := c.doRequestBody(http.MethodPost, "/logout", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RuntimeAction calls POST /admin/nodes/{name}/runtime/{action} (action is
// "start", "stop", or "restart") - the CLI's first mutating command,
// mirroring the Admin API's own dispatch-to-agent contract exactly (no
// business logic lives in the CLI - Law #6).
func (c *Client) RuntimeAction(node, action string) error {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/nodes/"+urlPathEscape(node)+"/runtime/"+action, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RuntimeLogs calls POST /admin/nodes/{name}/runtime/logs?lines=N - a pure
// read, still a POST since marbor injects driver/identifier into
// the agent-side request body, same as start/stop/restart. lines<=0 means
// "use the server-side default" - omitted from the query string.
func (c *Client) RuntimeLogs(node string, lines int) ([]string, error) {
	path := "/admin/nodes/" + urlPathEscape(node) + "/runtime/logs"
	if lines > 0 {
		path += fmt.Sprintf("?lines=%d", lines)
	}
	resp, err := c.doRequestBody(http.MethodPost, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out struct {
		Lines []string `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse runtime logs response: %v", err)
	}
	return out.Lines, nil
}

// NodeControlDiscovery mirrors the "discovered" object in GET
// /admin/nodes/{name}/control's response.
type NodeControlDiscovery struct {
	Driver     string   `json:"driver"`
	Identifier string   `json:"identifier"`
	Evidence   []string `json:"evidence"`
}

// NodeControlInfo mirrors admin.go's handleGetNodeControl response shape.
type NodeControlInfo struct {
	Node         string               `json:"node"`
	Configured   bool                 `json:"configured"`
	Driver       string               `json:"driver"`
	Identifier   string               `json:"identifier"`
	StartCommand string               `json:"start_command"`
	Discovered   NodeControlDiscovery `json:"discovered"`
}

// NodeControlProbe calls GET /admin/nodes/{name}/control - a read, so it
// uses doRequest's GET path rather than doRequestBody.
func (c *Client) NodeControlProbe(node string) (*NodeControlInfo, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/nodes/"+urlPathEscape(node)+"/control", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out NodeControlInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse control status response: %v", err)
	}
	return &out, nil
}

// NodeControlAccept calls POST /admin/nodes/{name}/control/accept - the
// operator's explicit confirmation of a discovered (or manually typed)
// control driver + identifier. startCommand is only meaningful for the
// Process driver's Start action; pass "" for every other driver.
func (c *Client) NodeControlAccept(node, driver, identifier, startCommand string) error {
	body := map[string]string{"driver": driver, "identifier": identifier}
	if startCommand != "" {
		body["start_command"] = startCommand
	}
	resp, err := c.doRequestBody(http.MethodPost, "/admin/nodes/"+urlPathEscape(node)+"/control/accept", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// urlPathEscape escapes name for use as a single path segment - node names
// don't contain "/" the way model names do, so no per-segment splitting is
// needed (unlike escapeModelPathSegments on the admin side).
func urlPathEscape(name string) string {
	return url.PathEscape(name)
}

// escapeModelPathSegments mirrors admin.go's function of the same name - a
// model name may itself contain "/" (e.g. an HF-style "namespace/repo" tag),
// which must survive as path separators, not be percent-encoded into %2F.
func escapeModelPathSegments(model string) string {
	parts := strings.Split(model, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// PullResult mirrors handleNodePull's 202-Accepted response body - the pull
// itself runs async server-side, so this only confirms the job started,
// same "one Admin API request" contract as RuntimeAction.
type PullResult struct {
	OK    bool   `json:"ok"`
	Node  string `json:"node"`
	Model string `json:"model"`
}

// PullModel calls POST /admin/nodes/{name}/pull - starts an async model pull,
// mirroring the UI's Models.tsx pull flow. Like the UI, this does not block
// for completion; the caller should poll `models list` or watch the
// dashboard's pull progress for the terminal outcome.
func (c *Client) PullModel(node, model string) (*PullResult, error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/nodes/"+urlPathEscape(node)+"/pull", map[string]string{"model": model})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out PullResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse pull response: %v", err)
	}
	return &out, nil
}

// DeleteNodeModel calls DELETE /admin/nodes/{name}/models/{model} -
// capability "models.delete", mirroring the UI's Models.tsx delete action.
func (c *Client) DeleteNodeModel(node, model string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/nodes/"+urlPathEscape(node)+"/models/"+escapeModelPathSegments(model), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// NodeModelEntry mirrors admin.go's nodeModelEntry - a single model in a
// node's local inventory (distinct from ModelEntry, which is the fleet-wide
// aggregate `models` uses).
type NodeModelEntry struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Source    string `json:"source"`
	Family    string `json:"family,omitempty"`
}

// NodeModels calls GET /admin/nodes/{name}/models - capability "models.list",
// the per-node local inventory (as opposed to Models(), the fleet-wide
// summary).
func (c *Client) NodeModels(node string) ([]NodeModelEntry, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/nodes/"+urlPathEscape(node)+"/models", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out struct {
		Models []NodeModelEntry `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse node models response: %v", err)
	}
	return out.Models, nil
}

// UnloadModel calls POST /admin/nodes/{name}/unload - evicts a model from a
// node's warm state, mirroring the UI's GPUNodes.tsx card action.
func (c *Client) UnloadModel(node, model string) error {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/nodes/"+urlPathEscape(node)+"/unload", map[string]string{"model": model})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// DrainResult mirrors handleDrainNode/handleUndrainNode's response body.
type DrainResult struct {
	Node     string `json:"node"`
	Draining bool   `json:"draining"`
	Reason   string `json:"reason,omitempty"`
}

// DrainNode calls POST /admin/nodes/{name}/drain - marks a node as draining
// (marbor-internal routing state; never sent to the Marbor Agent), mirroring the
// UI's GPUNodes.tsx "Drain" action.
func (c *Client) DrainNode(node, reason string) (*DrainResult, error) {
	var body map[string]string
	if reason != "" {
		body = map[string]string{"reason": reason}
	}
	resp, err := c.doRequestBody(http.MethodPost, "/admin/nodes/"+urlPathEscape(node)+"/drain", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out DrainResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse drain response: %v", err)
	}
	return &out, nil
}

// UndrainNode calls DELETE /admin/nodes/{name}/drain - reverses DrainNode,
// mirroring the UI's GPUNodes.tsx "Undrain" action.
func (c *Client) UndrainNode(node string) (*DrainResult, error) {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/nodes/"+urlPathEscape(node)+"/drain", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out DrainResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse undrain response: %v", err)
	}
	return &out, nil
}

// HealthCheckResult mirrors admin.go's nodeHealthCheckResult - an on-demand
// active liveness probe result, capability "runtime.health_check".
type HealthCheckResult struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
}

// HealthCheck calls GET /admin/nodes/{name}/health-check - mirroring the
// UI's GPUNodes.tsx checkNodeHealth action. A populated result with OK=false
// is a successful probe reporting a down runtime, not a request failure -
// only a transport/dispatch error returns a non-nil error.
func (c *Client) HealthCheck(node string) (*HealthCheckResult, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/nodes/"+urlPathEscape(node)+"/health-check", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out HealthCheckResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse health check response: %v", err)
	}
	return &out, nil
}

// HealthResp mirrors GET /health's response shape (admin.go handleHealth).
type HealthResp struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	ProxyPort     int    `json:"proxy_port"`
	UptimeSeconds int    `json:"uptime_seconds"`
	Nodes         struct {
		Total   int `json:"total"`
		Healthy int `json:"healthy"`
	} `json:"nodes"`
}

// Health calls GET /health (unauthenticated).
func (c *Client) Health() (*HealthResp, error) {
	resp, err := c.doRequest(http.MethodGet, "/health", false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out HealthResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse /health response: %v", err)
	}
	return &out, nil
}

// NodeResp mirrors admin.go's nodeResp - the fields a CLI table/JSON
// consumer cares about (a strict subset; unknown fields are ignored so this
// stays forward-compatible with new fields the server adds later).
type NodeResp struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Host          string            `json:"host"`
	Port          int               `json:"port"`
	GPUModel      string            `json:"gpuModel"`
	VRAMTotalMB   int64             `json:"vramTotalMB"`
	VRAMUsedMB    int64             `json:"vramUsedMB"`
	Runtime       string            `json:"runtime"`
	Health        string            `json:"health"`
	Draining      bool              `json:"draining"`
	LoadedModels  []json.RawMessage `json:"loadedModels"`
	ActiveConns   int32             `json:"activeConns"`
	RequestsTotal int64             `json:"requestsTotal"`
}

// PatchNodeTLSFingerprint calls PATCH /admin/v1/nodes/{name} with
// tls_fingerprint (headless enrollment confirmation) - matches
// router.NodePatch's snake_case JSON tag. fingerprint must be
// supplied by the caller (ultimately the operator, via the CLI's
// --fingerprint flag) - this method never probes or infers a value itself.
func (c *Client) PatchNodeTLSFingerprint(name, fingerprint string) error {
	resp, err := c.doRequestBody(http.MethodPatch, "/admin/v1/nodes/"+urlPathEscape(name),
		map[string]string{"tls_fingerprint": fingerprint})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// PatchNodeFieldsWithPtr is the visited-aware variant for `marbor nodes
// patch` (parallelism, vram-override) - sends every visited field
// in ONE PATCH request/body, matching how handlePatchNode already applies
// them together atomically server-side. Splitting these into separate
// sequential requests would reopen a partial-failure window the single-PATCH
// admin handler doesn't have (code review finding). nil means "flag
// not visited, no change"; a non-nil pointer to a zero-value ("" / 0 / an
// empty map) explicitly clears that field.
func (c *Client) PatchNodeFieldsWithPtr(name string, pType *string, pWidth *int, vramOverrides *map[string]int64) error {
	body := map[string]interface{}{}
	if pType != nil {
		if *pType == "" {
			body["parallelism_type"] = nil
		} else {
			body["parallelism_type"] = *pType
		}
	}
	if pWidth != nil {
		if *pWidth == 0 {
			body["parallelism_width"] = nil
		} else {
			body["parallelism_width"] = *pWidth
		}
	}
	if vramOverrides != nil {
		body["vram_overrides"] = *vramOverrides
	}
	resp, err := c.doRequestBody(http.MethodPatch, "/admin/v1/nodes/"+urlPathEscape(name), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// Nodes calls GET /admin/v1/nodes (session-authed).
func (c *Client) Nodes() ([]NodeResp, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/v1/nodes", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out []NodeResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse /admin/v1/nodes response: %v", err)
	}
	return out, nil
}

// NodeAddRequest mirrors config.NodeConfig's JSON shape for POST
// /admin/nodes - kept as a local DTO (this package's existing convention,
// see NodeResp) rather than importing internal/config.
type NodeAddRequest struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	GPUModel    string `json:"gpu_model,omitempty"`
	VRAMTotalMB int64  `json:"vram_total_mb,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
}

// AddNode calls POST /admin/nodes - fleet membership add, mirroring the UI's
// GPUNodes.tsx "Add Node" action. handleAddNode upserts by name in place, so
// this can also update an existing node's declared fields; the returned bool
// reports which happened (201 Created vs 200 OK), matching the server's own
// isUpdate distinction.
func (c *Client) AddNode(req NodeAddRequest) (created bool, err error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/nodes", req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusCreated, nil
}

// DeleteNode calls DELETE /admin/nodes/{name} - fleet membership remove
// (cascades marbor_agent + warmup settings server-side, see
// handleRemoveNode), mirroring the UI's GPUNodes.tsx "Remove" action.
func (c *Client) DeleteNode(name string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/nodes/"+urlPathEscape(name), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ScoreComponent mirrors one term of router.RoutingDecision.Components - a
// weighted factor contributing to a node's placement score.
type ScoreComponent struct {
	Name   string  `json:"name"`
	Raw    float64 `json:"raw"`
	Weight float64 `json:"weight"`
	Value  float64 `json:"value"`
}

// RoutingDecision mirrors router.RoutingDecision for per-request routing
// explainability. Kept as a local DTO (not an import of internal/router)
// following this package's existing convention of decoding Admin API JSON
// into its own response types (see NodeResp) rather than depending on
// internal packages.
type RoutingDecision struct {
	Node         string           `json:"node"`
	Reason       string           `json:"reason"`
	Detail       string           `json:"detail,omitempty"`
	AffinityLost bool             `json:"affinityLost,omitempty"`
	Score        float64          `json:"score,omitempty"`
	Components   []ScoreComponent `json:"components,omitempty"`
}

// ExplainRequest calls GET /admin/v1/requests/{id}/explain, returning the
// full routing explanation for one request id.
func (c *Client) ExplainRequest(id string) (*RoutingDecision, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/v1/requests/"+urlPathEscape(id)+"/explain", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out RoutingDecision
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse explain response: %v", err)
	}
	return &out, nil
}

// SpillCounterRow mirrors one row of GET /admin/v1/spill - a per-key,
// per-served_by request count. served_by is "local", a cloud provider's
// name, or "blocked" (a local_only policy rejection).
type SpillCounterRow struct {
	KeyName  string `json:"key_name"`
	ServedBy string `json:"served_by"`
	Requests int64  `json:"requests"`
}

// SpillCounters calls GET /admin/v1/spill, returning every (key_name,
// served_by) row fleet-wide.
func (c *Client) SpillCounters() ([]SpillCounterRow, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/v1/spill", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out []SpillCounterRow
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse spill counters response: %v", err)
	}
	return out, nil
}

// PatchKeyLocalOnly calls PATCH /admin/v1/keys/{name} with local_only, the
// the fail-closed policy toggle - matches auth.KeyPatch's snake_case JSON
// tag (handlePatchKey decodes into that struct, unlike handleAddKey which
// decodes into config.KeyConfig's camelCase tags).
func (c *Client) PatchKeyLocalOnly(name string, localOnly bool) error {
	resp, err := c.doRequestBody(http.MethodPatch, "/admin/v1/keys/"+urlPathEscape(name),
		map[string]bool{"local_only": localOnly})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// PatchKeyAllowLocalDegradation calls PATCH /admin/v1/keys/{name} with
// allow_local_degradation, the per-key policy gate on whether this key
// may be substituted with an operator-declared local alternate model -
// matches auth.KeyPatch's snake_case JSON tag, same as PatchKeyLocalOnly.
func (c *Client) PatchKeyAllowLocalDegradation(name string, allow bool) error {
	resp, err := c.doRequestBody(http.MethodPatch, "/admin/v1/keys/"+urlPathEscape(name),
		map[string]bool{"allow_local_degradation": allow})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// --- Keys: list/create/revoke/patch ---

// KeyResp mirrors admin.go's keyResp for LIST and mirrors config.KeyConfig for CREATE.
type KeyResp struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	Key                   string   `json:"key"`
	Created               string   `json:"created"`
	RequestsToday         int      `json:"requestsToday"`
	RequestsThisMonth     int      `json:"requestsThisMonth"`
	TokensThisMonth       int64    `json:"tokensThisMonth"`
	EstimatedCostUsd      float64  `json:"estimatedCostUsd"`
	RateLimit             int      `json:"rateLimit"`
	DailyLimit            int      `json:"dailyLimit"`
	MonthlyLimit          int      `json:"monthlyLimit"`
	DailyUsdCap           float64  `json:"dailyUsdCap,omitempty"`
	MonthlyUsdCap         float64  `json:"monthlyUsdCap,omitempty"`
	Status                string   `json:"status"`
	AllowedModels         []string `json:"allowedModels"`
	ExpiresAt             string   `json:"expiresAt,omitempty"`
	LocalOnly             bool     `json:"localOnly,omitempty"`
	AllowLocalDegradation bool     `json:"allowLocalDegradation,omitempty"`
}

// ListKeys calls GET /admin/v1/keys (session-authed).
func (c *Client) ListKeys() ([]KeyResp, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/v1/keys", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []KeyResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse keys response: %v", err)
	}
	if out == nil {
		out = []KeyResp{}
	}
	return out, nil
}

// KeyCreateRequest mirrors config.KeyConfig JSON shape for POST /admin/v1/keys.
type KeyCreateRequest struct {
	Name                  string   `json:"name"`
	Key                   string   `json:"key,omitempty"`
	RateLimit             int      `json:"rateLimit,omitempty"`
	DailyLimit            int      `json:"dailyLimit,omitempty"`
	MonthlyLimit          int      `json:"monthlyLimit,omitempty"`
	DailyUsdCap           float64  `json:"dailyUsdCap,omitempty"`
	MonthlyUsdCap         float64  `json:"monthlyUsdCap,omitempty"`
	Models                []string `json:"models,omitempty"`
	ExpiresAt             string   `json:"expiresAt,omitempty"`
	LocalOnly             bool     `json:"localOnly,omitempty"`
	AllowLocalDegradation bool     `json:"allowLocalDegradation,omitempty"`
}

// CreateKey calls POST /admin/v1/keys (session-authed).
func (c *Client) CreateKey(req KeyCreateRequest) (*KeyResp, error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/v1/keys", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out KeyResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse create key response: %v", err)
	}
	return &out, nil
}

// RevokeKey calls DELETE /admin/v1/keys/{name} (session-authed).
func (c *Client) RevokeKey(name string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/v1/keys/"+urlPathEscape(name), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// KeyPatch mirrors auth.KeyPatch for PATCH /admin/v1/keys/{name}.
type KeyPatch struct {
	RateLimit             *int      `json:"rate_limit,omitempty"`
	DailyLimit            *int      `json:"daily_limit,omitempty"`
	MonthlyLimit          *int      `json:"monthly_limit,omitempty"`
	DailyUsdCap           *float64  `json:"daily_usd_cap,omitempty"`
	MonthlyUsdCap         *float64  `json:"monthly_usd_cap,omitempty"`
	Models                *[]string `json:"models,omitempty"`
	ExpiresAt             *string   `json:"expires_at,omitempty"`
	LocalOnly             *bool     `json:"local_only,omitempty"`
	AllowLocalDegradation *bool     `json:"allow_local_degradation,omitempty"`
}

// PatchKey calls PATCH /admin/v1/keys/{name} (session-authed) with a full patch.
func (c *Client) PatchKey(name string, patch KeyPatch) error {
	resp, err := c.doRequestBody(http.MethodPatch, "/admin/v1/keys/"+urlPathEscape(name), patch)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// --- Users ---

// UserResp mirrors store.User JSON for GET/POST/PATCH users.
type UserResp struct {
	ID                 int64   `json:"id"`
	Username           string  `json:"username"`
	Email              string  `json:"email"`
	Role               string  `json:"role"`
	Status             string  `json:"status"`
	APIKeyName         string  `json:"api_key_name"`
	MustChangePassword bool    `json:"must_change_password"`
	CreatedAt          string  `json:"created_at"`
	ApprovedAt         *string `json:"approved_at,omitempty"`
	ApprovedBy         string  `json:"approved_by,omitempty"`
}

// ListUsers calls GET /admin/v1/users (session-authed).
func (c *Client) ListUsers() ([]UserResp, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/v1/users", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []UserResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse users response: %v", err)
	}
	if out == nil {
		out = []UserResp{}
	}
	return out, nil
}

// CreateUserRequest mirrors handleCreateUser JSON.
type CreateUserRequest struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role,omitempty"`
}

// CreateUserResp mirrors handleCreateUser 201 response: flat User plus initial_password.
type CreateUserResp struct {
	UserResp
	InitialPassword string `json:"initial_password"`
}

// CreateUser calls POST /admin/v1/users (session-authed).
func (c *Client) CreateUser(req CreateUserRequest) (*CreateUserResp, error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/v1/users", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out CreateUserResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse create user response: %v", err)
	}
	return &out, nil
}

// ApproveUserRequest mirrors handleApproveUser JSON.
type ApproveUserRequest struct {
	APIKeyName string `json:"api_key_name,omitempty"`
	CreateKey  *struct {
		Name         string   `json:"name,omitempty"`
		RateLimit    int      `json:"rate_limit_per_hour,omitempty"`
		DailyLimit   int      `json:"daily_limit,omitempty"`
		MonthlyLimit int      `json:"monthly_limit,omitempty"`
		Models       []string `json:"models,omitempty"`
	} `json:"create_key,omitempty"`
}

// ApproveUserResp mirrors handleApproveUser response.
type ApproveUserResp struct {
	User        UserResp `json:"user"`
	APIKeyValue string   `json:"api_key_value,omitempty"`
}

// ApproveUser calls POST /admin/v1/users/{id}/approve (session-authed).
func (c *Client) ApproveUser(id int64, req ApproveUserRequest) (*ApproveUserResp, error) {
	path := fmt.Sprintf("/admin/v1/users/%d/approve", id)
	resp, err := c.doRequestBody(http.MethodPost, path, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out ApproveUserResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse approve user response: %v", err)
	}
	return &out, nil
}

// SuspendUser calls POST /admin/v1/users/{id}/suspend (session-authed).
func (c *Client) SuspendUser(id int64) error {
	path := fmt.Sprintf("/admin/v1/users/%d/suspend", id)
	resp, err := c.doRequestBody(http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ResetUserPassword calls POST /admin/v1/users/{id}/reset-password (session-authed).
func (c *Client) ResetUserPassword(id int64) (string, error) {
	path := fmt.Sprintf("/admin/v1/users/%d/reset-password", id)
	resp, err := c.doRequestBody(http.MethodPost, path, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		InitialPassword string `json:"initial_password"`
		Password        string `json:"password"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", serverErrorf("could not parse reset password response: %v", err)
	}
	if out.InitialPassword != "" {
		return out.InitialPassword, nil
	}
	return out.Password, nil
}

// PatchUserRequest mirrors handlePatchUser JSON.
type PatchUserRequest struct {
	Email *string `json:"email,omitempty"`
	Role  *string `json:"role,omitempty"`
}

// PatchUser calls PATCH /admin/v1/users/{id} (session-authed).
func (c *Client) PatchUser(id int64, req PatchUserRequest) (*UserResp, error) {
	path := fmt.Sprintf("/admin/v1/users/%d", id)
	resp, err := c.doRequestBody(http.MethodPatch, path, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out UserResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse patch user response: %v", err)
	}
	return &out, nil
}

// DeleteUser calls DELETE /admin/v1/users/{id} (session-authed).
func (c *Client) DeleteUser(id int64) error {
	path := fmt.Sprintf("/admin/v1/users/%d", id)
	resp, err := c.doRequestBody(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ModelNodeInfo mirrors handleModels' nested nodeInfo struct.
type ModelNodeInfo struct {
	Name      string `json:"name"`
	Healthy   bool   `json:"healthy"`
	Digest    string `json:"digest,omitempty"`
	Warm      bool   `json:"warm"`
	VRAMBytes int64  `json:"vram_bytes,omitempty"`
	Runtime   string `json:"runtime,omitempty"`
}

// ModelEntry mirrors handleModels' modelEntry struct.
type ModelEntry struct {
	Name           string          `json:"name"`
	SizeVRAM       int64           `json:"size_vram"`
	SizeDisk       int64           `json:"size_disk,omitempty"`
	Nodes          []ModelNodeInfo `json:"nodes"`
	WarmCount      int             `json:"warm_count"`
	TotalNodes     int             `json:"total_nodes"`
	Family         string          `json:"family,omitempty"`
	DigestMismatch bool            `json:"digest_mismatch,omitempty"`
	TotalVRAMBytes int64           `json:"total_vram_bytes,omitempty"`
	DriftDetails   string          `json:"drift_details,omitempty"`
}

// ModelsResp mirrors GET /admin/v1/models' wrapped response shape.
type ModelsResp struct {
	Models       []ModelEntry `json:"models"`
	TotalModels  int          `json:"total_models"`
	TotalNodes   int          `json:"total_nodes"`
	HealthyNodes int          `json:"healthy_nodes"`
}

// Models calls GET /admin/v1/models (session-authed).
func (c *Client) Models() (*ModelsResp, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/v1/models", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out ModelsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse /admin/v1/models response: %v", err)
	}
	return &out, nil
}

// SystemAuditEntry mirrors the JSON returned by GET /admin/system-audit.
type SystemAuditEntry struct {
	Time     string `json:"time"`
	Username string `json:"username"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	Details  string `json:"details"`
	SourceIP string `json:"source_ip"`
}

// queryParams builds a url.Values from alternating name/value pairs,
// skipping any pair whose value is empty. Shared by every filtered-list
// Client method (SystemAuditFiltered, AuditQuery, ...) so the "only send
// non-empty optional filters" pattern lives in one place instead of being
// hand-copied per method (code review finding).
func queryParams(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			v.Set(pairs[i], pairs[i+1])
		}
	}
	return v
}

// SystemAuditFilter mirrors the query params for GET /admin/system-audit.
type SystemAuditFilter struct {
	From     string
	To       string
	Before   string
	Limit    int
	Kind     string
	Action   string
	User     string
	Target   string
	SourceIP string
}

// SystemAudit calls GET /admin/system-audit?limit=N. N <=0 uses server default.
func (c *Client) SystemAudit(limit int) ([]SystemAuditEntry, error) {
	return c.SystemAuditFiltered(SystemAuditFilter{Limit: limit})
}

// SystemAuditFiltered calls GET /admin/system-audit with enterprise filters.
func (c *Client) SystemAuditFiltered(f SystemAuditFilter) ([]SystemAuditEntry, error) {
	limit := ""
	if f.Limit > 0 {
		limit = fmt.Sprintf("%d", f.Limit)
	}
	params := queryParams(
		"from", f.From,
		"to", f.To,
		"before", f.Before,
		"limit", limit,
		"kind", f.Kind,
		"action", f.Action,
		"user", f.User,
		"target", f.Target,
		"source_ip", f.SourceIP,
	)
	path := "/admin/system-audit"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	resp, err := c.doRequest(http.MethodGet, path, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []SystemAuditEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse system audit response: %v", err)
	}
	return out, nil
}

// PredictiveDecision mirrors the shape returned by GET /admin/predictive/decisions.
type PredictiveDecision struct {
	Timestamp       string `json:"timestamp"`
	PredictedModel  string `json:"predicted_model"`
	TriggerModel    string `json:"trigger_model"`
	Node            string `json:"node"`
	WasAlreadyWarm  bool   `json:"was_already_warm"`
	WarmupTriggered bool   `json:"warmup_triggered"`
	TransitionCount int    `json:"transition_count"`
	Hour            int    `json:"hour"`
}

// PredictiveDecisions calls GET /admin/predictive/decisions.
func (c *Client) PredictiveDecisions() ([]PredictiveDecision, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/predictive/decisions", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var wrapper struct {
		Decisions []PredictiveDecision `json:"decisions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return nil, serverErrorf("could not parse predictive decisions response: %v", err)
	}
	if wrapper.Decisions == nil {
		return []PredictiveDecision{}, nil
	}
	return wrapper.Decisions, nil
}

// RequestEntry mirrors handleRequests' response entry shape (GET
// /admin/requests) - the dashboard's request log, newest first.
type RequestEntry struct {
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

// Requests calls GET /admin/requests - the full in-memory request log ring,
// newest first, mirroring the UI's Dashboard.tsx/Requests.tsx table.
func (c *Client) Requests() ([]RequestEntry, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/requests", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []RequestEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse /admin/requests response: %v", err)
	}
	return out, nil
}

// LiveRequestEntry mirrors admin.go's RequestLog shape (GET
// /admin/requests/live) - a raw in-flight/recent request row, distinct from
// RequestEntry's dashboard-formatted shape (different status semantics:
// Status here is the "warm/loading/error/aborted/cloud" label, not an HTTP
// code).
type LiveRequestEntry struct {
	ID            string    `json:"id"`
	ApiKey        string    `json:"apiKey"`
	SourceIP      string    `json:"sourceIP"`
	Model         string    `json:"model"`
	Node          string    `json:"routedTo"`
	Status        string    `json:"status"`
	HTTPStatus    int       `json:"httpStatus"`
	Latency       int       `json:"latency"`
	Tokens        int64     `json:"tokens"`
	TokensPerSec  float64   `json:"tokensPerSec"`
	Time          time.Time `json:"time"`
	RoutingReason string    `json:"routingReason,omitempty"`
}

// LiveRequests calls GET /admin/requests/live - the same bounded in-memory
// ring as Requests, in its raw (non-dashboard-formatted) shape, mirroring
// the UI's live-updating request widget.
func (c *Client) LiveRequests() ([]LiveRequestEntry, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/requests/live", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []LiveRequestEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse /admin/requests/live response: %v", err)
	}
	return out, nil
}

// AuditEntry mirrors internal/audit.Entry - a persisted, filterable request
// audit row (distinct from SystemAuditEntry, which covers operator actions
// like drain/agent/runtime, not individual proxied requests).
type AuditEntry struct {
	Time          time.Time `json:"time"`
	RequestID     string    `json:"request_id"`
	KeyName       string    `json:"key_name"`
	Model         string    `json:"model"`
	Node          string    `json:"node"`
	Status        string    `json:"status"`
	LatencyMs     int       `json:"latency_ms"`
	Cloud         bool      `json:"cloud"`
	CloudModel    string    `json:"cloud_model,omitempty"`
	RoutingReason string    `json:"routing_reason,omitempty"`
}

// AuditFilter mirrors the query params for GET /admin/audit.
type AuditFilter struct {
	Limit  int
	Model  string
	Key    string
	Node   string
	Status string // success | client_error | server_error
	Cloud  *bool
	Since  string // RFC3339
	Until  string // RFC3339
}

// AuditResult mirrors GET /admin/audit's wrapped response shape.
type AuditResult struct {
	Entries   []AuditEntry `json:"entries"`
	Total     int          `json:"total"`
	Truncated bool         `json:"truncated"`
}

// AuditQuery calls GET /admin/audit with the request audit log's own filter
// set (distinct from SystemAuditFiltered's operator-action filters), mirroring
// the UI's Requests.tsx audit view.
func (c *Client) AuditQuery(f AuditFilter) (*AuditResult, error) {
	limit := ""
	if f.Limit > 0 {
		limit = fmt.Sprintf("%d", f.Limit)
	}
	cloud := ""
	if f.Cloud != nil {
		cloud = fmt.Sprintf("%t", *f.Cloud)
	}
	params := queryParams(
		"limit", limit,
		"model", f.Model,
		"key", f.Key,
		"node", f.Node,
		"status", f.Status,
		"cloud", cloud,
		"since", f.Since,
		"until", f.Until,
	)
	path := "/admin/audit"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	resp, err := c.doRequest(http.MethodGet, path, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out AuditResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse /admin/audit response: %v", err)
	}
	return &out, nil
}

// NodeWarmupInfo mirrors handleGetNodeWarmup/handleSetNodeWarmup's response
// shape (GET/PUT /admin/nodes/{name}/warmup).
type NodeWarmupInfo struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models"`
}

// GetNodeWarmup calls GET /admin/nodes/{name}/warmup.
func (c *Client) GetNodeWarmup(node string) (*NodeWarmupInfo, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/nodes/"+urlPathEscape(node)+"/warmup", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out NodeWarmupInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse node warmup response: %v", err)
	}
	return &out, nil
}

// SetNodeWarmup calls PUT /admin/nodes/{name}/warmup.
func (c *Client) SetNodeWarmup(node string, enabled bool, models []string) (*NodeWarmupInfo, error) {
	resp, err := c.doRequestBody(http.MethodPut, "/admin/nodes/"+urlPathEscape(node)+"/warmup",
		map[string]interface{}{"enabled": enabled, "models": models})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out NodeWarmupInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse node warmup response: %v", err)
	}
	return &out, nil
}

// pinnedModelsResp is the shared response shape for both GET and PUT
// /admin/nodes/{name}/pinned (handleGetPinned/handleSetPinned echo the same
// {"models":[...]} envelope) - named once so a future field addition needs
// updating in exactly one place, not the two inline copies this used to be
// (code review finding).
type pinnedModelsResp struct {
	Models []string `json:"models"`
}

// GetPinned calls GET /admin/nodes/{name}/pinned - the node's never-evict
// model list.
func (c *Client) GetPinned(node string) ([]string, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/nodes/"+urlPathEscape(node)+"/pinned", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out pinnedModelsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse pinned response: %v", err)
	}
	return out.Models, nil
}

// SetPinned calls PUT /admin/nodes/{name}/pinned (whole-list replace).
func (c *Client) SetPinned(node string, models []string) ([]string, error) {
	resp, err := c.doRequestBody(http.MethodPut, "/admin/nodes/"+urlPathEscape(node)+"/pinned",
		map[string]interface{}{"models": models})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out pinnedModelsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse pinned response: %v", err)
	}
	return out.Models, nil
}

// SetNodePrewarm calls POST /admin/nodes/{name}/prewarm - disables/re-enables
// predictive prewarm for one node.
func (c *Client) SetNodePrewarm(node string, disabled bool) error {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/nodes/"+urlPathEscape(node)+"/prewarm",
		map[string]interface{}{"disabled": disabled})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// Schedule mirrors router.Schedule - a time-of-day warmup/unload/drain/
// undrain automation entry.
type Schedule struct {
	ID         string   `json:"id"`
	Action     string   `json:"action"`
	Node       string   `json:"node"`
	Models     []string `json:"models,omitempty"`
	At         string   `json:"at"`
	Days       []int    `json:"days,omitempty"`
	Enabled    bool     `json:"enabled"`
	LastRunAt  string   `json:"last_run_at,omitempty"`
	LastStatus string   `json:"last_status,omitempty"`
	LastError  string   `json:"last_error,omitempty"`
}

// ScheduleCreateRequest is the body for POST /admin/schedules.
type ScheduleCreateRequest struct {
	Action  string   `json:"action"`
	Node    string   `json:"node"`
	Models  []string `json:"models,omitempty"`
	At      string   `json:"at"`
	Days    []int    `json:"days,omitempty"`
	Enabled bool     `json:"enabled"`
}

// SchedulePatch is the body for PATCH /admin/schedules/{id} - nil fields are
// left unchanged server-side.
type SchedulePatch struct {
	Enabled *bool     `json:"enabled,omitempty"`
	Action  *string   `json:"action,omitempty"`
	Node    *string   `json:"node,omitempty"`
	Models  *[]string `json:"models,omitempty"`
	At      *string   `json:"at,omitempty"`
	Days    *[]int    `json:"days,omitempty"`
}

// ListSchedules calls GET /admin/schedules.
func (c *Client) ListSchedules() ([]Schedule, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/schedules", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []Schedule
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse schedules response: %v", err)
	}
	return out, nil
}

// CreateSchedule calls POST /admin/schedules.
func (c *Client) CreateSchedule(req ScheduleCreateRequest) (*Schedule, error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/schedules", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out Schedule
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse schedule response: %v", err)
	}
	return &out, nil
}

// PatchSchedule calls PATCH /admin/schedules/{id}.
func (c *Client) PatchSchedule(id string, patch SchedulePatch) (*Schedule, error) {
	resp, err := c.doRequestBody(http.MethodPatch, "/admin/schedules/"+urlPathEscape(id), patch)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out Schedule
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse schedule response: %v", err)
	}
	return &out, nil
}

// DeleteSchedule calls DELETE /admin/schedules/{id}.
func (c *Client) DeleteSchedule(id string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/schedules/"+urlPathEscape(id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RoutingRule mirrors config.RoutingRule.
type RoutingRule struct {
	ID         string `json:"id"`
	Priority   int    `json:"priority"`
	Condition  string `json:"condition"`
	TargetNode string `json:"targetNode"`
	Strategy   string `json:"strategy,omitempty"`
	Enabled    bool   `json:"enabled"`
}

// RoutingRules calls GET /admin/routing/rules.
func (c *Client) RoutingRules() ([]RoutingRule, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/routing/rules", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []RoutingRule
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse routing rules response: %v", err)
	}
	return out, nil
}

// AddRoutingRule calls POST /admin/routing/rules.
func (c *Client) AddRoutingRule(rule RoutingRule) error {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/routing/rules", rule)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RemoveRoutingRule calls DELETE /admin/routing/rules/{id}.
func (c *Client) RemoveRoutingRule(id string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/routing/rules/"+urlPathEscape(id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ToggleRoutingRule calls PUT /admin/routing/rules/{id}/toggle.
func (c *Client) ToggleRoutingRule(id string) error {
	resp, err := c.doRequestBody(http.MethodPut, "/admin/routing/rules/"+urlPathEscape(id)+"/toggle", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RoutingStrategy calls GET /admin/routing/strategy.
func (c *Client) RoutingStrategy() (string, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/routing/strategy", true)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Strategy string `json:"strategy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", serverErrorf("could not parse routing strategy response: %v", err)
	}
	return out.Strategy, nil
}

// SetRoutingStrategy calls PUT /admin/routing/strategy.
func (c *Client) SetRoutingStrategy(strategy string) error {
	resp, err := c.doRequestBody(http.MethodPut, "/admin/routing/strategy", map[string]string{"strategy": strategy})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// CloudProvider mirrors handleCloudProviders' response shape (GET
// /admin/cloud/providers) - the API key itself is NEVER returned by the
// server, so this type deliberately has no APIKey field.
type CloudProvider struct {
	Name            string  `json:"name"`
	Provider        string  `json:"provider"`
	BaseURL         string  `json:"base_url"`
	DefaultModel    string  `json:"default_model"`
	CostPer1KTokens float64 `json:"cost_per_1k_tokens"`
	Enabled         bool    `json:"enabled"`
	Priority        int     `json:"priority"`
}

// CloudProviderRequest is the body for POST /admin/cloud/providers and PUT
// /admin/cloud/providers/{name}. APIKey is write-only: pass "" on an update
// to leave the currently stored key unchanged (handleUpdateCloudProvider
// preserves the existing value for both "" and the UI's "***" placeholder -
// never round-trip the real secret through a read).
type CloudProviderRequest struct {
	Name            string  `json:"name"`
	Provider        string  `json:"provider"`
	BaseURL         string  `json:"base_url"`
	APIKey          string  `json:"api_key,omitempty"`
	DefaultModel    string  `json:"default_model,omitempty"`
	CostPer1KTokens float64 `json:"cost_per_1k_tokens,omitempty"`
	Enabled         bool    `json:"enabled"`
	Priority        int     `json:"priority,omitempty"`
}

// CloudProviders calls GET /admin/cloud/providers.
func (c *Client) CloudProviders() ([]CloudProvider, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/cloud/providers", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []CloudProvider
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse cloud providers response: %v", err)
	}
	return out, nil
}

// AddCloudProvider calls POST /admin/cloud/providers.
func (c *Client) AddCloudProvider(req CloudProviderRequest) error {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/cloud/providers", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// UpdateCloudProvider calls PUT /admin/cloud/providers/{name}.
func (c *Client) UpdateCloudProvider(name string, req CloudProviderRequest) error {
	resp, err := c.doRequestBody(http.MethodPut, "/admin/cloud/providers/"+urlPathEscape(name), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// DeleteCloudProvider calls DELETE /admin/cloud/providers/{name}.
func (c *Client) DeleteCloudProvider(name string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/cloud/providers/"+urlPathEscape(name), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ReorderCloudProviders calls PUT /admin/cloud/providers/reorder.
func (c *Client) ReorderCloudProviders(order []string) error {
	resp, err := c.doRequestBody(http.MethodPut, "/admin/cloud/providers/reorder", map[string]interface{}{"order": order})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// TestCloudProvider calls POST /admin/cloud/providers/test - verifies a
// base_url+api_key pair authenticates against the provider before saving.
// The api_key is sent once, on the wire, for this one-shot probe only - it
// is never persisted or echoed back by this call.
func (c *Client) TestCloudProvider(provider, baseURL, apiKey string) error {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/cloud/providers/test", map[string]string{
		"provider": provider, "base_url": baseURL, "api_key": apiKey,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// CloudBudgetEntry mirrors admin.go's BudgetEntry.
type CloudBudgetEntry struct {
	Name         string  `json:"name,omitempty"`
	DailySpent   float64 `json:"dailySpent"`
	DailyCap     float64 `json:"dailyCap"`
	DailyPct     float64 `json:"dailyPct"`
	MonthlySpent float64 `json:"monthlySpent"`
	MonthlyCap   float64 `json:"monthlyCap"`
	MonthlyPct   float64 `json:"monthlyPct"`
}

// CloudBudgetStatus mirrors admin.go's CloudBudgetStatusResp.
type CloudBudgetStatus struct {
	SoftBudgetPct float64            `json:"softBudgetPct"`
	Global        CloudBudgetEntry   `json:"global"`
	PerKey        []CloudBudgetEntry `json:"perKey"`
}

// CloudBudgetStatus calls GET /admin/cloud-budget-status.
func (c *Client) CloudBudgetStatus() (*CloudBudgetStatus, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/cloud-budget-status", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out CloudBudgetStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse cloud budget status response: %v", err)
	}
	return &out, nil
}

// Favorites calls GET /admin/favorites - the calling user's starred model
// ids.
func (c *Client) Favorites() ([]string, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/favorites", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		ModelIDs []string `json:"model_ids"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse favorites response: %v", err)
	}
	return out.ModelIDs, nil
}

// AddFavorite calls POST /admin/favorites - stars a model for the calling user.
func (c *Client) AddFavorite(modelID string) error {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/favorites", map[string]string{"model_id": modelID})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RemoveFavorite calls DELETE /admin/favorites/{modelId} - unstars a model.
func (c *Client) RemoveFavorite(modelID string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/favorites/"+escapeModelPathSegments(modelID), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// GetModelConfig calls GET /admin/model-config?model=X&node=Y, returning the
// raw JSON body (store.ModelConfig has ~40 optional per-runtime sampling/
// load-time fields - see internal/store/store.go - kept as raw JSON here
// rather than mirrored field-by-field into a CLI-local type, matching this
// method's read-only "pass through what the server sent" role; SetModelConfig
// takes the same raw-JSON shape for writes, deliberately not exposing 40
// individual flags).
func (c *Client) GetModelConfig(model, node string) (json.RawMessage, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/model-config?model="+url.QueryEscape(model)+"&node="+url.QueryEscape(node), true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, serverErrorf("could not read model config response: %v", err)
	}
	return body, nil
}

// SetModelConfig calls PUT /admin/model-config with a raw JSON body (the
// full store.ModelConfig shape - model+node required, every other field is
// forwarded as-is; see GetModelConfig's doc comment for why this stays raw
// JSON instead of ~40 individual flags).
func (c *Client) SetModelConfig(rawBody json.RawMessage) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodPut, c.BaseURL+"/admin/model-config", bytes.NewReader(rawBody))
	if err != nil {
		return nil, userErrorf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, serverErrorf("could not reach %s: %v", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, userErrorf("%s", readErrorMessage(resp.Body))
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, serverErrorf("could not read model config response: %v", err)
	}
	return out, nil
}

// DeleteModelConfig calls DELETE /admin/model-config?model=X&node=Y.
func (c *Client) DeleteModelConfig(model, node string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/model-config?model="+url.QueryEscape(model)+"&node="+url.QueryEscape(node), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ListModelConfigs calls GET /admin/model-configs, returning the raw
// {"configs":[...]} JSON body (see GetModelConfig's doc comment for why
// this package keeps ModelConfig as raw JSON rather than a mirrored type).
func (c *Client) ListModelConfigs() (json.RawMessage, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/model-configs", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, serverErrorf("could not read model configs response: %v", err)
	}
	return body, nil
}

// ModelConfigCapabilities calls GET /admin/model-config/capabilities -
// per-runtime list of which ModelConfig fields actually take effect.
func (c *Client) ModelConfigCapabilities() (map[string][]string, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/model-config/capabilities", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse model config capabilities response: %v", err)
	}
	return out, nil
}

// ModelFitEntry mirrors handleModelFit's modelFitEntry.
type ModelFitEntry struct {
	Name              string `json:"name"`
	SizeBytes         int64  `json:"size_bytes"`
	VRAMEstimateBytes int64  `json:"vram_estimate_bytes"`
	Fit               string `json:"fit"`
	Loaded            bool   `json:"loaded"`
}

// NodeFitEntry mirrors handleModelFit's nodeFitEntry.
type NodeFitEntry struct {
	Name           string          `json:"name"`
	URL            string          `json:"url"`
	VRAMFreeBytes  int64           `json:"vram_free_bytes"`
	VRAMTotalBytes int64           `json:"vram_total_bytes"`
	VRAMSource     string          `json:"vram_source"`
	Models         []ModelFitEntry `json:"models"`
}

// ModelFit calls GET /admin/nodes/model-fit - per-node VRAM fit analysis
// for models.
func (c *Client) ModelFit() ([]NodeFitEntry, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/nodes/model-fit", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Nodes []NodeFitEntry `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse model fit response: %v", err)
	}
	return out.Nodes, nil
}

// ModelCatalog calls GET /admin/models/catalog, returning the raw JSON body
// - a nested {"catalog":[...],"nodes":[...]} shape (fleet-aware HF/local
// model catalog with per-node fit and downloaded-status data). Kept as raw
// JSON rather than mirrored field-by-field, same rationale as
// GetModelConfig: a large, purely-informational discovery response where a
// hand-mirrored type would only need updating every time the browsing UI
// gains a field.
func (c *Client) ModelCatalog() (json.RawMessage, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/models/catalog", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ModelSearchOpts mirrors the query params for GET /admin/models/search.
type ModelSearchOpts struct {
	Query   string
	Runtime string
	Sort    string // downloads (default) | likes | newest | oldest
}

// ModelSearch calls GET /admin/models/search, returning the raw JSON array
// of Hugging Face search results (see ModelCatalog's doc comment for why
// this stays raw JSON).
func (c *Client) ModelSearch(opts ModelSearchOpts) (json.RawMessage, error) {
	params := queryParams("q", opts.Query, "runtime", opts.Runtime, "sort", opts.Sort)
	path := "/admin/models/search"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	resp, err := c.doRequest(http.MethodGet, path, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ModelRepoOpts mirrors the query params for GET /admin/models/repo.
type ModelRepoOpts struct {
	ID      string // required: owner/name
	Node    string
	Runtime string
	CtxLen  int
}

// ModelRepo calls GET /admin/models/repo, returning the raw JSON repo detail
// (see ModelCatalog's doc comment for why this stays raw JSON).
func (c *Client) ModelRepo(opts ModelRepoOpts) (json.RawMessage, error) {
	ctxLen := ""
	if opts.CtxLen > 0 {
		ctxLen = fmt.Sprintf("%d", opts.CtxLen)
	}
	params := queryParams("id", opts.ID, "node", opts.Node, "runtime", opts.Runtime, "ctx", ctxLen)
	path := "/admin/models/repo"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	resp, err := c.doRequest(http.MethodGet, path, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// BackupFileInfo mirrors admin.go's backupFileInfo.
type BackupFileInfo struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
}

// ListBackups calls GET /admin/backup/list.
func (c *Client) ListBackups() ([]BackupFileInfo, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/backup/list", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Backups []BackupFileInfo `json:"backups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse backup list response: %v", err)
	}
	return out.Backups, nil
}

// RestoreBackup calls POST /admin/backup/restore - restarts marbor onto the
// named backup file (destructive; the caller is responsible for
// confirming with the operator first).
func (c *Client) RestoreBackup(filename string) error {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/backup/restore", map[string]string{"filename": filename})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// backupContentDispositionRE extracts the filename from a
// `Content-Disposition: attachment; filename="..."` response header.
var backupContentDispositionRE = regexp.MustCompile(`filename="([^"]+)"`)

// BackupNow calls POST /admin/backup - an on-demand backup streamed back as
// a binary .db download. Returns the server-suggested filename (parsed from
// Content-Disposition, falling back to a timestamped name if absent/
// unparsable) and the raw file bytes; the caller writes it to disk.
func (c *Client) BackupNow() (filename string, data []byte, err error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/backup", nil)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, serverErrorf("could not read backup response: %v", err)
	}
	filename = "marbor-backup.db"
	if m := backupContentDispositionRE.FindStringSubmatch(resp.Header.Get("Content-Disposition")); len(m) == 2 {
		filename = m[1]
	}
	return filename, data, nil
}

// UploadBackup calls POST /admin/backup/upload with r's content as a
// multipart/form-data "file" field, mirroring the UI's Settings.tsx upload
// picker. filename is only used for the multipart part's own filename field
// (informational to the server, which renames the staged file to its own
// timestamped shape regardless - see handleUploadBackup).
func (c *Client) UploadBackup(filename string, r io.Reader) error {
	if c.Token == "" {
		return userErrorf("authentication required: run 'marbor login', or pass --username/--password (or set MARBOR_USERNAME+MARBOR_PASSWORD)")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return userErrorf("building upload request: %v", err)
	}
	if _, err := io.Copy(part, r); err != nil {
		return userErrorf("reading local file: %v", err)
	}
	if err := mw.Close(); err != nil {
		return userErrorf("building upload request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/admin/backup/upload", &buf)
	if err != nil {
		return userErrorf("building request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return serverErrorf("could not reach %s: %v", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return userErrorf("%s", readErrorMessage(resp.Body))
	}
	return nil
}

// AnalyticsSummary calls GET /admin/analytics, returning the raw JSON body
// (hourly analytics + per-model stats - a large, purely-informational
// dashboard aggregate; see ModelCatalog's doc comment for why this stays
// raw JSON).
func (c *Client) AnalyticsSummary() (json.RawMessage, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/analytics", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// AnalyticsExport downloads GET /admin/analytics/export?type=X, returning
// the raw CSV bytes and the server-suggested filename.
func (c *Client) AnalyticsExport(exportType, format string) (filename string, data []byte, err error) {
	params := queryParams("type", exportType, "format", format)
	path := "/admin/analytics/export"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	resp, err := c.doRequest(http.MethodGet, path, true)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, serverErrorf("could not read analytics export response: %v", err)
	}
	filename = "analytics-export.json"
	if format == "csv" {
		filename = "analytics-export.csv"
	}
	if m := backupContentDispositionRE.FindStringSubmatch(resp.Header.Get("Content-Disposition")); len(m) == 2 {
		filename = m[1]
	}
	return filename, data, nil
}

// CloudSavings mirrors GET /admin/metrics/savings' response shape.
// CloudSpentUSD and SavedUSD are nullable pointers because the server sends
// JSON null (not 0.0) when requests exist but no token data was ever parsed -
// a non-pointer float64 would silently decode that null as a fabricated $0.00.
type CloudSavings struct {
	LocalRequests int64    `json:"local_requests"`
	CloudRequests int64    `json:"cloud_requests"`
	TotalRequests int64    `json:"total_requests"`
	CloudSpentUSD *float64 `json:"cloud_spent_usd"`
	SavedUSD      *float64 `json:"saved_usd"`
	Since         string   `json:"since"`
}

// Savings calls GET /admin/metrics/savings.
func (c *Client) Savings() (*CloudSavings, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/metrics/savings", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out CloudSavings
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse savings response: %v", err)
	}
	return &out, nil
}

// MetricsSummary mirrors GET /admin/metrics/summary's response shape.
type MetricsSummary struct {
	ActiveRequests int32   `json:"active_requests"`
	NodesOnline    int     `json:"nodes_online"`
	NodesDraining  int     `json:"nodes_draining"`
	TotalNodes     int     `json:"total_nodes"`
	QueueDepth     int     `json:"queue_depth"`
	AvgLatency     float64 `json:"avg_latency"`
	TokensPerMin   int64   `json:"tokens_per_min"`
	ColdStarts     int64   `json:"cold_starts"`
	WarmHitRatio   float64 `json:"warm_hit_ratio"`
}

// MetricsSummary calls GET /admin/metrics/summary - the dashboard's top
// summary strip (nodes online, active requests, latency, tokens/min).
func (c *Client) MetricsSummary() (*MetricsSummary, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/metrics/summary", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out MetricsSummary
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse metrics summary response: %v", err)
	}
	return &out, nil
}

// WarmupStatus mirrors GET /admin/warmup's response shape.
type WarmupStatus struct {
	Enabled                 bool     `json:"enabled"`
	IntervalMs              int      `json:"interval_ms"`
	KeepAlive               string   `json:"keep_alive"`
	Models                  []string `json:"models"`
	PredictiveEngineEnabled bool     `json:"predictive_engine_enabled"`
}

// GetWarmupStatus calls GET /admin/warmup - global warmup engine status.
func (c *Client) GetWarmupStatus() (*WarmupStatus, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/warmup", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out WarmupStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse warmup status response: %v", err)
	}
	return &out, nil
}

// SetPredictiveEngine calls PUT /admin/warmup/predictive.
func (c *Client) SetPredictiveEngine(enabled bool) error {
	resp, err := c.doRequestBody(http.MethodPut, "/admin/warmup/predictive", map[string]bool{"enabled": enabled})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// WarmupPing calls POST /admin/warmup/ping - manually triggers a warmup cycle.
func (c *Client) WarmupPing() error {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/warmup/ping", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// SystemInfo calls GET /admin/system-info, returning the raw JSON body -
// control-plane host system info plus per-node GPU summary (see
// ModelCatalog's doc comment for why this stays raw JSON).
func (c *Client) SystemInfo() (json.RawMessage, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/system-info", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ConfigReloadResult mirrors POST /admin/config/reload's response shape.
type ConfigReloadResult struct {
	Reloaded       bool `json:"reloaded"`
	AuthKeys       int  `json:"auth_keys"`
	NodesAdded     int  `json:"nodes_added"`
	NodesRemoved   int  `json:"nodes_removed"`
	CloudProviders int  `json:"cloud_providers"`
}

// ConfigReload calls POST /admin/config/reload - re-syncs live
// router/auth state from SQLite.
func (c *Client) ConfigReload() (*ConfigReloadResult, error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/config/reload", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out ConfigReloadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse config reload response: %v", err)
	}
	return &out, nil
}

// PendingUserCount calls GET /admin/v1/users/pending-count.
func (c *Client) PendingUserCount() (int, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/v1/users/pending-count", true)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, serverErrorf("could not parse pending user count response: %v", err)
	}
	return out.Count, nil
}

// PullJobSnapshot mirrors admin.go's pullJobSnapshot - one active/recent
// model pull job.
type PullJobSnapshot struct {
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

// ActivePulls calls GET /admin/pulls - every currently-active pull job
// across the fleet, mirroring the UI's pullProgress.ts tracking.
// "models pull-progress" (a single node+model) filters this same list
// client-side rather than following the separate SSE progress stream, per
// the "one CLI command is exactly one Admin API request" contract.
func (c *Client) ActivePulls() ([]PullJobSnapshot, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/pulls", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []PullJobSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse active pulls response: %v", err)
	}
	return out, nil
}

// CancelPull calls DELETE /admin/nodes/{name}/pull?model=X - cancels an
// in-flight pull.
func (c *Client) CancelPull(node, model string) (cancelled bool, err error) {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/nodes/"+urlPathEscape(node)+"/pull?model="+url.QueryEscape(model), nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		OK        bool `json:"ok"`
		Cancelled bool `json:"cancelled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, serverErrorf("could not parse cancel pull response: %v", err)
	}
	return out.Cancelled, nil
}

// RunBenchmark calls POST /admin/benchmark/run - starts a hardware
// benchmark job, returning its job id.
func (c *Client) RunBenchmark(node, model string, n int) (jobID string, err error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/benchmark/run", map[string]interface{}{
		"node": node, "model": model, "n": n,
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", serverErrorf("could not parse benchmark run response: %v", err)
	}
	return out.JobID, nil
}

// CancelBenchmark calls DELETE /admin/benchmark/{id}.
func (c *Client) CancelBenchmark(jobID string) (cancelled bool, err error) {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/benchmark/"+urlPathEscape(jobID), nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		Cancelled bool `json:"cancelled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, serverErrorf("could not parse cancel benchmark response: %v", err)
	}
	return out.Cancelled, nil
}

// BenchmarkProgress calls GET /admin/benchmark/{id}/progress - a
// point-in-time snapshot from the SSE progress stream's underlying data,
// not a live follow (see ActivePulls' doc comment for the same one-request
// rationale). The endpoint itself is SSE-only, so this reads the first
// event off the stream and returns its raw JSON payload.
func (c *Client) BenchmarkProgress(jobID string) (json.RawMessage, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/benchmark/"+urlPathEscape(jobID)+"/progress", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return json.RawMessage(data), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, serverErrorf("could not read benchmark progress stream: %v", err)
	}
	return nil, serverErrorf("benchmark progress stream closed with no data")
}

// BenchmarkRuns calls GET /admin/benchmark/runs, returning the raw JSON
// {"runs":[...]} body - store.BenchmarkRun has many percentile/TPOT fields,
// kept as raw JSON rather than mirrored (see ModelCatalog's doc comment).
func (c *Client) BenchmarkRuns() (json.RawMessage, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/benchmark/runs", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// MarborAgentInfo mirrors handleGetMarborAgent's response shape - never
// carries the token (only enable/regenerate return that, once).
type MarborAgentInfo struct {
	Node    string `json:"node"`
	Enabled bool   `json:"enabled"`
	Port    int    `json:"port"`
	Scope   string `json:"scope,omitempty"`
	Scheme  string `json:"scheme,omitempty"`
}

// GetMarborAgent calls GET /admin/nodes/{name}/agent.
func (c *Client) GetMarborAgent(node string) (*MarborAgentInfo, error) {
	resp, err := c.doRequest(http.MethodGet, "/admin/nodes/"+urlPathEscape(node)+"/agent", true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out MarborAgentInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse marbor agent response: %v", err)
	}
	return &out, nil
}

// MarborAgentEnableResult mirrors handleEnableMarborAgent/
// handleRegenerateMarborAgentToken's response shape - the ONLY response
// that ever carries the plaintext enrollment install command/token -
// never persisted or echoed back by GetMarborAgent.
type MarborAgentEnableResult struct {
	Node                  string `json:"node"`
	Enabled               bool   `json:"enabled"`
	Port                  int    `json:"port"`
	Scheme                string `json:"scheme"`
	Token                 string `json:"token"`
	InstallCommand        string `json:"install_command"`
	InstallCommandWindows string `json:"install_command_windows"`
}

// EnableMarborAgent calls POST /admin/nodes/{name}/agent - enables or
// reconfigures the marbor agent for a node. scheme empty means "keep the
// host's existing scheme, or http on first enable" (see handler doc).
func (c *Client) EnableMarborAgent(node string, port int, scheme string) (*MarborAgentEnableResult, error) {
	body := map[string]interface{}{"port": port}
	if scheme != "" {
		body["scheme"] = scheme
	}
	resp, err := c.doRequestBody(http.MethodPost, "/admin/nodes/"+urlPathEscape(node)+"/agent", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out MarborAgentEnableResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse enable marbor agent response: %v", err)
	}
	return &out, nil
}

// DisableMarborAgent calls DELETE /admin/nodes/{name}/agent.
func (c *Client) DisableMarborAgent(node string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/nodes/"+urlPathEscape(node)+"/agent", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RegenerateMarborAgentToken calls POST /admin/nodes/{name}/agent/regenerate.
func (c *Client) RegenerateMarborAgentToken(node string) (*MarborAgentEnableResult, error) {
	resp, err := c.doRequestBody(http.MethodPost, "/admin/nodes/"+urlPathEscape(node)+"/agent/regenerate", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out MarborAgentEnableResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, serverErrorf("could not parse regenerate marbor agent token response: %v", err)
	}
	// handleRegenerateMarborAgentToken's response body has no "enabled" key
	// (unlike handleEnableMarborAgent's), so out.Enabled would otherwise
	// decode to Go's zero-value false here - not a real "now disabled"
	// signal (the endpoint 404s before this point unless the agent is
	// already enabled), just an absent field. Set explicitly rather than
	// let a --json caller read a fabricated false (code review finding).
	out.Enabled = true
	return &out, nil
}

// ClearNodeControl calls DELETE /admin/nodes/{name}/control - clears the
// accepted control driver config.
func (c *Client) ClearNodeControl(node string) error {
	resp, err := c.doRequestBody(http.MethodDelete, "/admin/nodes/"+urlPathEscape(node)+"/control", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ChangePassword calls POST /change-password (session-role-agnostic, same
// route choice as Logout's doc comment explains). currentPassword may be
// empty only on a forced first-login change (MustChangePassword=true) - the
// server itself enforces that, not this client. The endpoint invalidates
// every existing session for the user and issues a fresh one, so this
// updates c.Token in place from the response's Set-Cookie, mirroring Login.
func (c *Client) ChangePassword(currentPassword, newPassword string) error {
	payload, err := json.Marshal(struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}{currentPassword, newPassword})
	if err != nil {
		return userErrorf("building request body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/change-password", bytes.NewReader(payload))
	if err != nil {
		return userErrorf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return serverErrorf("could not reach %s: %v", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return userErrorf("%s", readErrorMessage(resp.Body))
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "marbor_session" {
			c.Token = cookie.Value
			break
		}
	}
	return nil
}

// SkipPasswordChange calls POST /skip-password-change - dismisses the
// forced-password-change prompt for this session only, up to
// maxSkipPasswordChanges times server-side. Also rotates the session token.
func (c *Client) SkipPasswordChange() error {
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/skip-password-change", nil)
	if err != nil {
		return userErrorf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return serverErrorf("could not reach %s: %v", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return userErrorf("%s", readErrorMessage(resp.Body))
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "marbor_session" {
			c.Token = cookie.Value
			break
		}
	}
	return nil
}

// savedSessionHint returns a suffix clarifying that a 401/403 came from a
// saved-session token specifically (as opposed to an explicit --token the
// operator just typed), which is the case where "run it again" is the right
// next step. Empty for every other token source.
func (c *Client) savedSessionHint() string {
	if c.usingSavedSession {
		return " - run marbor login again"
	}
	return ""
}

// readErrorMessage best-effort extracts the "error" field the Admin API's
// JSON error responses use ({"error":"..."}), falling back to raw body text.
func readErrorMessage(r io.Reader) string {
	body, err := io.ReadAll(r)
	if err != nil {
		return "(could not read response body)"
	}
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
		return parsed.Error
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "(empty response body)"
	}
	return trimmed
}
