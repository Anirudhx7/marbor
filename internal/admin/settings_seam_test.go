package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

func settingsServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, auth.NewMiddleware(cfg.Auth), cfg)
}

func getSettingsMap(t *testing.T, s *Server) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The settings page reads lowercase proxy.port and auth.enabled; the config
// structs must not leak Go field names into the JSON.
func TestAdmin_SettingsJSONUsesLowercaseNames(t *testing.T) {
	cfg := config.Config{}
	cfg.Proxy.Port = 12345
	cfg.Auth.Enabled = config.BoolPtr(true)
	s := settingsServer(t, cfg)
	m := getSettingsMap(t, s)

	var proxy, authM map[string]json.RawMessage
	_ = json.Unmarshal(m["proxy"], &proxy)
	_ = json.Unmarshal(m["auth"], &authM)
	if string(proxy["port"]) != "12345" {
		t.Errorf("proxy.port = %s, want 12345 (keys: %v)", proxy["port"], proxy)
	}
	if _, bad := proxy["Port"]; bad {
		t.Error("proxy has PascalCase Port key")
	}
	if string(authM["enabled"]) != "true" {
		t.Errorf("auth.enabled = %s, want true", authM["enabled"])
	}
	if _, bad := authM["Enabled"]; bad {
		t.Error("auth has PascalCase Enabled key")
	}
}

// A routine save of an unrelated field, shaped like the UI payload, must not
// reset a custom proxy port.
func TestAdmin_SettingsSaveKeepsCustomProxyPort(t *testing.T) {
	cfg := config.Config{}
	cfg.Proxy.Port = 12345
	cfg.Auth.Keys = []config.KeyConfig{{Name: "k", Key: "sk-keep-me"}}
	s := settingsServer(t, cfg)

	m := getSettingsMap(t, s)
	body, _ := json.Marshal(m)
	req := httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code >= 300 {
		t.Fatalf("PUT settings = %d: %s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	got := s.cfg.Proxy.Port
	s.mu.RUnlock()
	s.mu.RLock()
	keys := s.cfg.Auth.Keys
	s.mu.RUnlock()
	if len(keys) != 1 || keys[0].Key != "sk-keep-me" {
		t.Errorf("auth keys after settings save = %+v, want the original key kept", keys)
	}
	if got != 12345 {
		t.Errorf("proxy port after unrelated save = %d, want 12345", got)
	}
}

// The settings response must never carry API key secrets.
func TestAdmin_SettingsDoesNotExposeAPIKeySecrets(t *testing.T) {
	cfg := config.Config{}
	cfg.Auth.Keys = []config.KeyConfig{{Name: "k", Key: "sk-probe-1"}}
	s := settingsServer(t, cfg)
	m := getSettingsMap(t, s)
	if strings.Contains(string(m["auth"]), "sk-probe-1") {
		t.Fatalf("settings response exposes an API key secret: %s", m["auth"])
	}
}

// GET must not return key secrets anywhere in the body, and must not mask the
// live config's cloud provider keys as a side effect.
func TestAdmin_SettingsGetHidesKeysAndDoesNotMutateLiveConfig(t *testing.T) {
	cfg := config.Config{}
	cfg.Auth.Keys = []config.KeyConfig{{Name: "k", Key: "sk-secret-xyz"}}
	cfg.CloudProviders = []config.CloudProvider{{Name: "openai", APIKey: "sk-cloud-real", Enabled: true}}
	s := settingsServer(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "sk-secret-xyz") || strings.Contains(body, "sk-cloud-real") {
		t.Errorf("settings body leaks a secret: %s", body)
	}
	s.mu.RLock()
	got := s.cfg.CloudProviders[0].APIKey
	s.mu.RUnlock()
	if got != "sk-cloud-real" {
		t.Errorf("live cloud provider key = %q after GET, want it unchanged", got)
	}
}

// The settings save has no authentication switch: auth.enabled in the request
// body is ignored and the running value is kept, whatever the body carries.
func TestAdmin_SettingsSaveIgnoresAuthEnabled(t *testing.T) {
	cases := []struct {
		name  string
		start bool
		auth  string // raw JSON for the "auth" key; "" omits the key entirely
	}{
		{"true to false", true, `{"enabled":false}`},
		{"false to true", false, `{"enabled":true}`},
		{"true with no auth key", true, ""},
		{"false with no auth key", false, ""},
		{"true with null", true, `{"enabled":null}`},
		{"false with null", false, `{"enabled":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Auth.Enabled = config.BoolPtr(tc.start)
			s := settingsServer(t, cfg)

			m := getSettingsMap(t, s)
			if tc.auth == "" {
				delete(m, "auth")
			} else {
				m["auth"] = json.RawMessage(tc.auth)
			}
			body, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body))
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code >= 300 {
				t.Fatalf("PUT settings = %d: %s", rec.Code, rec.Body.String())
			}

			s.mu.RLock()
			got := s.cfg.Auth.Enabled
			s.mu.RUnlock()
			if got == nil || *got != tc.start {
				t.Errorf("live auth.enabled after PUT = %v, want unchanged %v", got, tc.start)
			}

			var reported struct {
				Enabled *bool `json:"enabled"`
			}
			if err := json.Unmarshal(getSettingsMap(t, s)["auth"], &reported); err != nil {
				t.Fatal(err)
			}
			if reported.Enabled == nil || *reported.Enabled != tc.start {
				t.Errorf("GET reports auth.enabled = %v, want %v", reported.Enabled, tc.start)
			}

			// A save must not open the admin API to unauthenticated callers.
			unauth := httptest.NewRecorder()
			s.Handler().ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
			if unauth.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated GET /admin/settings after PUT = %d, want 401", unauth.Code)
			}
		})
	}
}
