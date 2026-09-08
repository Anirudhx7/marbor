package admin

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// errCloudProvidersStore wraps a real store.Store and fails AllCloudProviders
// on demand, without touching any other method - used to prove ReloadFromStore
// leaves live state untouched when a later stage's read fails.
type errCloudProvidersStore struct {
	store.Store
	fail bool
}

func (e *errCloudProvidersStore) AllCloudProviders() ([]store.CloudProviderRecord, error) {
	if e.fail {
		return nil, fmt.Errorf("injected AllCloudProviders failure")
	}
	return e.Store.AllCloudProviders()
}

func newReloadTestServer(t *testing.T, st store.Store) *Server {
	t.Helper()
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	a := auth.NewMiddleware(config.AuthConfig{})
	return NewServer(r, a, config.Config{}, st)
}

func routerHasNode(r *router.Router, name string) bool {
	for _, n := range r.Nodes() {
		if n.Name == name {
			return true
		}
	}
	return false
}

// TestHandleConfigReload_WritesSystemAuditRow verifies the P0 gap: reload was
// the only mutating admin op with no system-audit row.
func TestHandleConfigReload_WritesSystemAuditRow(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "reload-audit.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s := newReloadTestServer(t, st)

	req := httptest.NewRequest("POST", "/admin/config/reload", nil)
	w := httptest.NewRecorder()
	s.handleConfigReload(w, req)

	if w.Code != 200 {
		t.Fatalf("handleConfigReload: status = %d, body = %s", w.Code, w.Body.String())
	}

	entries, err := st.QuerySystemAuditLog(10)
	if err != nil {
		t.Fatalf("QuerySystemAuditLog: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "config_reload" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a config_reload system-audit row, got %+v", entries)
	}
}

// TestReloadFromStore_AppliesLiveState changes a node in the store and proves
// a reload picks it up in the live router state.
func TestReloadFromStore_AppliesLiveState(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "reload-apply.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	s := newReloadTestServer(t, st)

	if err := st.UpsertNode(store.NodeRecord{Name: "n1", URL: "http://127.0.0.1:11434", Runtime: "ollama"}); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	added, removed, _, _, err := s.ReloadFromStore()
	if err != nil {
		t.Fatalf("ReloadFromStore: %v", err)
	}
	if added != 1 || removed != 0 {
		t.Fatalf("ReloadFromStore: added=%d removed=%d, want added=1 removed=0", added, removed)
	}
	if !routerHasNode(s.router, "n1") {
		t.Fatal("expected router to hold node n1 after reload")
	}
}

// TestReloadFromStore_FailureKeepsPriorState proves the fixed load-then-apply
// ordering: a failure loading a later stage (cloud providers) must not have
// mutated an earlier stage (auth) that already succeeded its own load, and
// must not have synced nodes either (nodes load after cloud providers).
func TestReloadFromStore_FailureKeepsPriorState(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "reload-fail.db")
	realSt, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { realSt.Close() })

	if err := realSt.UpsertNode(store.NodeRecord{Name: "n1", URL: "http://127.0.0.1:11434", Runtime: "ollama"}); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	failing := &errCloudProvidersStore{Store: realSt, fail: true}
	s := newReloadTestServer(t, failing)

	if routerHasNode(s.router, "n1") {
		t.Fatal("test setup: node n1 must not exist in the router before reload")
	}

	_, _, _, _, err = s.ReloadFromStore()
	if err == nil {
		t.Fatal("expected ReloadFromStore to fail when AllCloudProviders errors")
	}

	if routerHasNode(s.router, "n1") {
		t.Fatal("ReloadFromStore must not apply nodes when an earlier stage failed to load")
	}

	// A second reload with the failure cleared must succeed and apply
	// everything, proving the store wasn't left in some broken partial state.
	failing.fail = false
	added, _, _, _, err := s.ReloadFromStore()
	if err != nil {
		t.Fatalf("ReloadFromStore after clearing failure: %v", err)
	}
	if added != 1 {
		t.Fatalf("ReloadFromStore after clearing failure: added=%d, want 1", added)
	}
	if !routerHasNode(s.router, "n1") {
		t.Fatal("expected router to hold node n1 after the successful retry")
	}
}
