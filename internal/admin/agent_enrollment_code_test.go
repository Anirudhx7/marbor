package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// The enable and regenerate responses carry the bare enrollment code next to
// install_command, so automation can build its own fixed-shape install command
// instead of running a server-supplied string through a shell.
func TestAgentEnableAndRegenerateReturnEnrollmentCode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "enroll-code.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "test-node", URL: "http://127.0.0.1:11434", Runtime: "ollama"},
	}, nil)
	s := NewServer(r, nil, config.Config{}, st)

	call := func(h http.HandlerFunc, path string, body []byte) map[string]interface{} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
		req.SetPathValue("name", "test-node")
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, w.Code, w.Body.String())
		}
		var m map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	}
	check := func(label string, m map[string]interface{}) {
		code, _ := m["enrollment_code"].(string)
		if code == "" {
			t.Fatalf("%s: enrollment_code missing or empty: %v", label, m)
		}
		if cmd, _ := m["install_command"].(string); !strings.Contains(cmd, "MARBOR_ENROLL="+code) {
			t.Errorf("%s: install_command does not carry the same code", label)
		}
	}
	body, _ := json.Marshal(map[string]int{"port": 9300})
	check("enable", call(s.handleEnableMarborAgent, "/admin/nodes/test-node/agent", body))
	check("regenerate", call(s.handleRegenerateMarborAgentToken, "/admin/nodes/test-node/agent/regenerate", nil))
}
