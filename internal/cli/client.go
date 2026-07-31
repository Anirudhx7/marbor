// Package cli implements the mesh CLI - a thin client of the Admin API.
// Per operational-interfaces.md, the CLI never talks to a Node Agent
// directly; every command is exactly one Admin API request.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Exit codes per operational-interfaces.md 5.2. 3 (partial success) doesn't
// apply until batch operations exist.
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
		MustChangePassword bool `json:"must_change_password"`
	}
	// Best-effort: a malformed body here doesn't invalidate a successful
	// login (the cookie is still authoritative), so decode errors are not
	// fatal - just skip the must-change-password fast-path below.
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(bodyBytes, &respBody)

	for _, cookie := range resp.Cookies() {
		if cookie.Name == "mesh_session" {
			c.Token = cookie.Value
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
			return nil, userErrorf("authentication required: pass --token, or --username/--password (or MESH_TOKEN / MESH_USERNAME+MESH_PASSWORD)")
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
		return nil, authErrorf("%s", readErrorMessage(resp.Body))
	case resp.StatusCode == http.StatusServiceUnavailable:
		// GET /health returns 503 with a still-decodable body to signal a
		// degraded (not down) mesh, not a hard failure - let the caller
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

// ModelNodeInfo mirrors handleModels' nested nodeInfo struct.
type ModelNodeInfo struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Digest  string `json:"digest,omitempty"`
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
