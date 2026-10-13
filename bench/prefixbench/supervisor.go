package main

// supervisor.go - starts a fresh router process for one run, registers the
// engines, completes first-login setup, creates an API key and applies the
// arm's routing settings through the admin API. A fresh process and database
// per run means no routing state (prefix history, counters) leaks between runs.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	proxyPort = 11434
	adminPort = 8080
	// metricsPort is bound by the router too; all three must be free before a start.
	metricsPort = 9090
)

type routerSession struct {
	ProxyURL string
	AdminURL string
	APIKey   string
	cookie   string
	cmd      *exec.Cmd
	// Settings is the effective settings document read back after applying
	// the arm's overrides, kept so a run records what was actually in force.
	Settings json.RawMessage
	logFile  *os.File
}

func portFree(p int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func waitPortsFree(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if portFree(proxyPort) && portFree(adminPort) && portFree(metricsPort) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("ports %d/%d/%d still busy after %s (is another router running?)", proxyPort, adminPort, metricsPort, d)
}

func randomPassword() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "Bench-" + hex.EncodeToString(b)
}

// adminDo performs one admin API call.
func adminDo(c *http.Client, method, url, cookie string, body any) (int, []byte, http.Header, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, data, resp.Header, nil
}

func (s *routerSession) admin(c *http.Client, method, path string, body any) (int, []byte, error) {
	code, data, _, err := adminDo(c, method, s.AdminURL+path, s.cookie, body)
	return code, data, err
}

// login returns the session cookie pair ("name=value").
func login(c *http.Client, adminURL, user, pass string) (cookie string, mustChange bool, err error) {
	code, data, hdr, err := adminDo(c, http.MethodPost, adminURL+"/admin/login", "", map[string]string{"username": user, "password": pass})
	if err != nil {
		return "", false, err
	}
	if code != http.StatusOK {
		return "", false, fmt.Errorf("login: HTTP %d: %s", code, trim(data))
	}
	var out struct {
		MustChange bool `json:"must_change_password"`
	}
	_ = json.Unmarshal(data, &out)
	sc := hdr.Get("Set-Cookie")
	if sc == "" {
		return "", false, errors.New("login: no session cookie in response")
	}
	for i := 0; i < len(sc); i++ {
		if sc[i] == ';' {
			sc = sc[:i]
			break
		}
	}
	return sc, out.MustChange, nil
}

func trim(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}

// startRouter launches the router for one run. Callers must call stop().
func startRouter(ctx context.Context, e *experiment, a armSpec, dir string) (*routerSession, error) {
	bin := e.binFor(a)
	if err := waitPortsFree(ctx, 30*time.Second); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db := filepath.Join(dir, "router.db")
	seed := []string{"-db", db}
	for _, en := range e.Engines {
		seed = append(seed, "-seed-node", fmt.Sprintf("name=%s,url=%s,runtime=%s", en.Name, en.URL, e.Runtime))
	}
	seedCmd := exec.CommandContext(ctx, bin, seed...)
	seedCmd.Dir = dir
	if out, err := seedCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("seeding engines: %v: %s", err, trim(out))
	}

	logf, err := os.Create(filepath.Join(dir, "router.log"))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "-db", db)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("starting router: %w", err)
	}
	s := &routerSession{
		ProxyURL: fmt.Sprintf("http://127.0.0.1:%d", proxyPort),
		AdminURL: fmt.Sprintf("http://127.0.0.1:%d", adminPort),
		cmd:      cmd, logFile: logf,
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	if err := s.bootstrap(ctx, hc, e, a); err != nil {
		s.stop()
		return nil, err
	}
	return s, nil
}

func (s *routerSession) bootstrap(ctx context.Context, hc *http.Client, e *experiment, a armSpec) error {
	var cookie string
	var must bool
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for {
		if cookie, must, err = login(hc, s.AdminURL, "admin", "admin"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("router did not accept the first login within 30s: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if must {
		pass := randomPassword()
		code, data, _, cerr := adminDo(hc, http.MethodPost, s.AdminURL+"/admin/change-password", cookie,
			map[string]string{"current_password": "admin", "new_password": pass})
		if cerr != nil || code != http.StatusOK {
			return fmt.Errorf("first-login password change failed: HTTP %d %v %s", code, cerr, trim(data))
		}
		if cookie, _, err = login(hc, s.AdminURL, "admin", pass); err != nil {
			return err
		}
	}
	s.cookie = cookie

	code, data, err := s.admin(hc, http.MethodPost, "/admin/keys", map[string]string{"name": "bench"})
	if err != nil || code != http.StatusCreated {
		return fmt.Errorf("creating api key: HTTP %d %v %s", code, err, trim(data))
	}
	var k struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &k); err != nil || k.Key == "" {
		return fmt.Errorf("api key response had no key: %s", trim(data))
	}
	s.APIKey = k.Key

	if err := s.applySettings(hc, e, a); err != nil {
		return err
	}
	return s.waitNodesHealthy(ctx, hc, e)
}

// applySettings merges RouterSet and the arm's settings into the routing
// section, writes the document back and verifies every override was applied.
func (s *routerSession) applySettings(hc *http.Client, e *experiment, a armSpec) error {
	code, data, err := s.admin(hc, http.MethodGet, "/admin/settings", nil)
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("reading settings: HTTP %d %v", code, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	routing, ok := doc["routing"].(map[string]any)
	if !ok {
		return errors.New("settings document has no routing section")
	}
	want := map[string]any{}
	for k, v := range e.RouterSet {
		want[k] = v
	}
	for k, v := range a.Settings {
		want[k] = v
	}
	for k, v := range want {
		routing[k] = v
	}
	if code, data, err = s.admin(hc, http.MethodPut, "/admin/settings", doc); err != nil || code != http.StatusOK {
		return fmt.Errorf("writing settings: HTTP %d %v %s", code, err, trim(data))
	}
	if code, data, err = s.admin(hc, http.MethodGet, "/admin/settings", nil); err != nil || code != http.StatusOK {
		return fmt.Errorf("re-reading settings: HTTP %d %v", code, err)
	}
	var after map[string]any
	if err := json.Unmarshal(data, &after); err != nil {
		return err
	}
	got, _ := after["routing"].(map[string]any)
	for k, v := range want {
		if fmt.Sprint(got[k]) != fmt.Sprint(v) {
			return fmt.Errorf("routing.%s: wanted %v, router reports %v (setting did not apply)", k, v, got[k])
		}
	}
	s.Settings = data
	return nil
}

func (s *routerSession) waitNodesHealthy(ctx context.Context, hc *http.Client, e *experiment) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, data, err := s.admin(hc, http.MethodGet, "/admin/nodes", nil)
		if err == nil && code == http.StatusOK {
			var nodes []struct {
				Name   string `json:"name"`
				Health string `json:"health"`
			}
			if json.Unmarshal(data, &nodes) == nil {
				healthy := 0
				for _, n := range nodes {
					if n.Health == "healthy" {
						healthy++
					}
				}
				if healthy >= len(e.Engines) {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return errors.New("engines did not all become healthy within 60s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *routerSession) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	if s.logFile != nil {
		s.logFile.Close()
	}
}
