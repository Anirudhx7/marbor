package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

func putRawSettings(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader([]byte(body))))
	return rec
}

func mustPut(t *testing.T, s *Server, body string) {
	t.Helper()
	if rec := putRawSettings(t, s, body); rec.Code != http.StatusOK {
		t.Fatalf("PUT %s = %d, want 200; body: %s", body, rec.Code, rec.Body.String())
	}
}

func TestUpdateSettings_FallbackChainsApplyToLiveRouter(t *testing.T) {
	s := chainsServer(t)
	mustPut(t, s, `{"routing":{"fallback_chains":{"big":["mid","small"]}}}`)
	if got := s.router.FallbackChainFor("big"); !reflect.DeepEqual(got, []string{"mid", "small"}) {
		t.Fatalf("router fallback chain after PUT = %v, want [mid small]", got)
	}
	mustPut(t, s, `{"routing":{"fallback_chains":{"big":["small"]}}}`)
	if got := s.router.FallbackChainFor("big"); !reflect.DeepEqual(got, []string{"small"}) {
		t.Errorf("router fallback chain after second PUT = %v, want [small]", got)
	}
}

func TestUpdateSettings_DegradationChainsApplyToLiveRouter(t *testing.T) {
	s := chainsServer(t)
	mustPut(t, s, `{"routing":{"local_degradation_chains":{"big":["small"]}}}`)
	if got := s.router.LocalDegradationChainFor("big"); !reflect.DeepEqual(got, []string{"small"}) {
		t.Fatalf("router degradation chain after PUT = %v, want [small]", got)
	}
	if got := s.router.FallbackChainFor("big"); got != nil {
		t.Errorf("fallback chain leaked from a degradation PUT: %v", got)
	}
}

func TestUpdateSettings_PartialPutKeepsOtherChain(t *testing.T) {
	s := chainsServer(t)
	mustPut(t, s, `{"routing":{"fallback_chains":{"a":["b"]},"local_degradation_chains":{"a":["c"]}}}`)
	mustPut(t, s, `{"routing":{"fallback_chains":{"a":["z"]}}}`)
	if got := s.router.FallbackChainFor("a"); !reflect.DeepEqual(got, []string{"z"}) {
		t.Errorf("fallback chain = %v, want [z]", got)
	}
	if got := s.router.LocalDegradationChainFor("a"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("degradation chain changed by a fallback-only PUT: %v", got)
	}
}

func TestUpdateSettings_PutOmittingChainsLeavesRouterChains(t *testing.T) {
	s := chainsServer(t)
	mustPut(t, s, `{"routing":{"fallback_chains":{"a":["b"]},"local_degradation_chains":{"a":["c"]}}}`)
	mustPut(t, s, `{"routing":{"max_retries":4}}`)
	if got := s.router.FallbackChainFor("a"); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("fallback chain after unrelated PUT = %v, want [b]", got)
	}
	if got := s.router.LocalDegradationChainFor("a"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("degradation chain after unrelated PUT = %v, want [c]", got)
	}
}

func TestUpdateSettings_NullClearsChains(t *testing.T) {
	s := chainsServer(t)
	mustPut(t, s, `{"routing":{"fallback_chains":{"a":["b"]},"local_degradation_chains":{"a":["c"]}}}`)
	if s.router.FallbackChainFor("a") == nil || s.router.LocalDegradationChainFor("a") == nil {
		t.Fatal("chains were not set before the null PUT")
	}
	mustPut(t, s, `{"routing":{"fallback_chains":null,"local_degradation_chains":null}}`)
	if got := s.router.FallbackChainFor("a"); got != nil {
		t.Errorf("fallback chain after null = %v, want nil", got)
	}
	if got := s.router.LocalDegradationChainFor("a"); got != nil {
		t.Errorf("degradation chain after null = %v, want nil", got)
	}
}

func TestUpdateSettings_RejectedPutLeavesRouterChains(t *testing.T) {
	s := chainsServer(t)
	mustPut(t, s, `{"routing":{"fallback_chains":{"a":["b"]},"local_degradation_chains":{"a":["c"]}}}`)
	rec := putRawSettings(t, s, `{"routing":{"fallback_chains":{"a":["changed"]},"local_degradation_chains":{"loop":["loop"]}}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-referencing chain PUT = %d, want 400", rec.Code)
	}
	if got := s.router.FallbackChainFor("a"); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("fallback chain after rejected PUT = %v, want [b]", got)
	}
	if got := s.router.LocalDegradationChainFor("loop"); got != nil {
		t.Errorf("rejected degradation chain reached the router: %v", got)
	}
}

func TestReloadFromStore_AppliesStoredChains(t *testing.T) {
	s := chainsServer(t)
	if err := store.SetJSONSetting(s.st, "routing_fallback_chains", map[string][]string{"a": {"b", "c"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetJSONSetting(s.st, "routing_local_degradation_chains", map[string][]string{"a": {"d"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatalf("ReloadFromStore: %v", err)
	}
	if got := s.router.FallbackChainFor("a"); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("router fallback chain after reload = %v", got)
	}
	if got := s.router.LocalDegradationChainFor("a"); !reflect.DeepEqual(got, []string{"d"}) {
		t.Errorf("router degradation chain after reload = %v", got)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !reflect.DeepEqual(s.cfg.Routing.FallbackChains["a"], []string{"b", "c"}) ||
		!reflect.DeepEqual(s.cfg.Routing.LocalDegradationChains["a"], []string{"d"}) {
		t.Errorf("s.cfg chains not updated by reload: %v / %v", s.cfg.Routing.FallbackChains, s.cfg.Routing.LocalDegradationChains)
	}
}

func TestReloadFromStore_AbsentRowsKeepCurrentChains(t *testing.T) {
	s := chainsServer(t)
	// Fresh install: no rows, no chains, reload must not fail or invent any.
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatalf("ReloadFromStore on empty store: %v", err)
	}
	if s.router.FallbackChainFor("a") != nil {
		t.Fatal("empty store produced a chain")
	}
	// Chains held in memory with no stored row survive a reload.
	s.router.SetFallbackChains(map[string][]string{"a": {"b"}})
	s.router.SetLocalDegradationChains(map[string][]string{"a": {"c"}})
	s.mu.Lock()
	s.cfg.Routing.FallbackChains = map[string][]string{"a": {"b"}}
	s.cfg.Routing.LocalDegradationChains = map[string][]string{"a": {"c"}}
	s.mu.Unlock()
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatalf("ReloadFromStore: %v", err)
	}
	if got := s.router.FallbackChainFor("a"); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("absent row cleared the fallback chain: %v", got)
	}
	if got := s.router.LocalDegradationChainFor("a"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("absent row cleared the degradation chain: %v", got)
	}
}

func TestReloadFromStore_NullRowClearsChain(t *testing.T) {
	s := chainsServer(t)
	mustPut(t, s, `{"routing":{"fallback_chains":{"a":["b"]},"local_degradation_chains":{"a":["c"]}}}`)
	if got := s.router.FallbackChainFor("a"); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("fallback chain before null row = %v, want [b]", got)
	}
	if err := s.st.SetSetting("routing_fallback_chains", "null"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatalf("ReloadFromStore: %v", err)
	}
	if got := s.router.FallbackChainFor("a"); got != nil {
		t.Errorf("null row left fallback chain %v", got)
	}
	if got := s.router.LocalDegradationChainFor("a"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("degradation chain changed by a fallback null row: %v", got)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.cfg.Routing.FallbackChains) != 0 {
		t.Errorf("s.cfg fallback chains not cleared by a null row: %v", s.cfg.Routing.FallbackChains)
	}
}

func TestReloadFromStore_OnePresentRowKeepsTheAbsentOne(t *testing.T) {
	s := chainsServer(t)
	s.router.SetLocalDegradationChains(map[string][]string{"a": {"c"}})
	s.mu.Lock()
	s.cfg.Routing.LocalDegradationChains = map[string][]string{"a": {"c"}}
	s.mu.Unlock()
	if err := store.SetJSONSetting(s.st, "routing_fallback_chains", map[string][]string{"a": {"new"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatalf("ReloadFromStore: %v", err)
	}
	if got := s.router.FallbackChainFor("a"); !reflect.DeepEqual(got, []string{"new"}) {
		t.Errorf("present fallback row not applied: %v", got)
	}
	if got := s.router.LocalDegradationChainFor("a"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("absent degradation row cleared the chain: %v", got)
	}
}

func TestUpdateSettings_MutatingCfgAfterPutDoesNotLeakToRouter(t *testing.T) {
	s := chainsServer(t)
	mustPut(t, s, `{"routing":{"fallback_chains":{"a":["b"]},"local_degradation_chains":{"a":["c"]}}}`)
	s.mu.Lock()
	s.cfg.Routing.FallbackChains["a"][0] = "mutated"
	s.cfg.Routing.FallbackChains["z"] = []string{"new"}
	s.cfg.Routing.LocalDegradationChains["a"][0] = "mutated"
	s.mu.Unlock()
	if got := s.router.FallbackChainFor("a"); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("router fallback chain follows s.cfg mutation: %v", got)
	}
	if got := s.router.LocalDegradationChainFor("a"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("router degradation chain follows s.cfg mutation: %v", got)
	}
	if s.router.FallbackChainFor("z") != nil {
		t.Error("key added to s.cfg after PUT is visible on the router")
	}
}

func TestReloadFromStore_CorruptChainRowAppliesNothing(t *testing.T) {
	for _, corruptKey := range []string{"routing_fallback_chains", "routing_local_degradation_chains"} {
		t.Run(corruptKey, func(t *testing.T) {
			s := chainsServer(t)
			mustPut(t, s, `{"routing":{"fallback_chains":{"a":["b"]},"local_degradation_chains":{"a":["c"]},"cloud_only_models":["x"]}}`)
			// A valid new value in one row and garbage in the other.
			good, bad := "routing_local_degradation_chains", "routing_fallback_chains"
			if corruptKey == "routing_local_degradation_chains" {
				good, bad = "routing_fallback_chains", "routing_local_degradation_chains"
			}
			if err := store.SetJSONSetting(s.st, good, map[string][]string{"a": {"new"}}); err != nil {
				t.Fatal(err)
			}
			if err := s.st.SetSetting(bad, "{not json"); err != nil {
				t.Fatal(err)
			}
			if err := s.st.SetSetting("routing_cloud_only_models", `["changed"]`); err != nil {
				t.Fatal(err)
			}
			// Rows that a successful reload would apply: a new node and cloud.
			if err := s.st.UpsertNode(store.NodeRecord{Name: "extra", URL: "http://localhost:11999", Runtime: "ollama"}); err != nil {
				t.Fatal(err)
			}
			if err := s.st.UpsertCloudProvider(store.CloudProviderRecord{Name: "cp-extra", Provider: "openai", BaseURL: "https://example.invalid", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			nodeCount := len(s.router.Nodes())
			s.mu.RLock()
			cfgNodes, cfgClouds, cfgKeys := len(s.cfg.Nodes), len(s.cfg.CloudProviders), len(s.cfg.Auth.Keys)
			s.mu.RUnlock()

			_, _, _, _, err := s.ReloadFromStore()
			if err == nil || !strings.Contains(err.Error(), "chain") {
				t.Fatalf("ReloadFromStore error = %v, want a chains error", err)
			}
			if got := s.router.FallbackChainFor("a"); !reflect.DeepEqual(got, []string{"b"}) {
				t.Errorf("fallback chain changed by a failed reload: %v", got)
			}
			if got := s.router.LocalDegradationChainFor("a"); !reflect.DeepEqual(got, []string{"c"}) {
				t.Errorf("degradation chain changed by a failed reload: %v", got)
			}
			if got := s.router.CloudOnlyModels(); !reflect.DeepEqual(got, []string{"x"}) {
				t.Errorf("cloud-only list changed by a failed reload: %v", got)
			}
			if got := len(s.router.Nodes()); got != nodeCount {
				t.Errorf("router nodes changed by a failed reload: %d, want %d", got, nodeCount)
			}
			s.mu.RLock()
			defer s.mu.RUnlock()
			if !reflect.DeepEqual(s.cfg.Routing.FallbackChains["a"], []string{"b"}) {
				t.Errorf("s.cfg fallback chains changed by a failed reload: %v", s.cfg.Routing.FallbackChains)
			}
			if !reflect.DeepEqual(s.cfg.Routing.LocalDegradationChains["a"], []string{"c"}) {
				t.Errorf("s.cfg degradation chains changed by a failed reload: %v", s.cfg.Routing.LocalDegradationChains)
			}
			if !reflect.DeepEqual(s.cfg.Routing.CloudOnlyModels, []string{"x"}) {
				t.Errorf("s.cfg cloud-only changed by a failed reload: %v", s.cfg.Routing.CloudOnlyModels)
			}
			if len(s.cfg.Nodes) != cfgNodes || len(s.cfg.CloudProviders) != cfgClouds || len(s.cfg.Auth.Keys) != cfgKeys {
				t.Errorf("s.cfg nodes/clouds/keys changed by a failed reload: %d/%d/%d", len(s.cfg.Nodes), len(s.cfg.CloudProviders), len(s.cfg.Auth.Keys))
			}
		})
	}
}

func TestChains_ReloadRacingPutStaysConsistent(t *testing.T) {
	s := chainsServer(t)
	values := []string{"p", "q"}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		v := values[i%2]
		wg.Add(2)
		go func() {
			defer wg.Done()
			if rec := putRawSettings(t, s, `{"routing":{"fallback_chains":{"a":["`+v+`"]}}}`); rec.Code != http.StatusOK {
				t.Errorf("PUT = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
		}()
		go func() {
			defer wg.Done()
			if _, _, _, _, err := s.ReloadFromStore(); err != nil {
				t.Errorf("ReloadFromStore: %v", err)
			}
		}()
	}
	wg.Wait()
	s.mu.RLock()
	want := s.cfg.Routing.FallbackChains["a"]
	s.mu.RUnlock()
	got := s.router.FallbackChainFor("a")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("router chain %v diverged from s.cfg chain %v", got, want)
	}
	if !reflect.DeepEqual(got, []string{"p"}) && !reflect.DeepEqual(got, []string{"q"}) {
		t.Errorf("final chain %v is not one of the written values", got)
	}
}

// routerNodeNames returns the sorted names of the live router nodes.
func routerNodeNames(s *Server) []string {
	var names []string
	for _, n := range s.router.Nodes() {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return names
}

// cfgIdentities returns the sorted node, cloud provider and key names in
// s.cfg. The caller holds s.mu.
func cfgIdentities(s *Server) (nodes, clouds, keys []string) {
	for _, n := range s.cfg.Nodes {
		nodes = append(nodes, n.Name)
	}
	for _, c := range s.cfg.CloudProviders {
		clouds = append(clouds, c.Name)
	}
	for _, k := range s.cfg.Auth.Keys {
		keys = append(keys, k.Name)
	}
	sort.Strings(nodes)
	sort.Strings(clouds)
	sort.Strings(keys)
	return nodes, clouds, keys
}

// chainsServer is a real-store server with an auth middleware, so
// ReloadFromStore (which reloads keys) can run against it.
func chainsServer(t *testing.T) *Server {
	t.Helper()
	s, _ := queueSettingsServer(t, openCloudOnlyStore(t))
	return s
}
