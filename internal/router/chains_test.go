package router

import (
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

func newChainRouter(t *testing.T, cfg config.RoutingConfig) *Router {
	t.Helper()
	cfg.Strategy = "warm-first"
	return New(cfg, nil, nil)
}

func TestSetFallbackChains_AppliesLive(t *testing.T) {
	r := newChainRouter(t, config.RoutingConfig{})
	if got := r.FallbackChainFor("big"); got != nil {
		t.Fatalf("before set = %v, want nil", got)
	}
	r.SetFallbackChains(map[string][]string{"big": {"mid", "small"}})
	got := r.FallbackChainFor("big")
	if len(got) != 2 || got[0] != "mid" || got[1] != "small" {
		t.Fatalf("after set = %v, want [mid small]", got)
	}
	r.SetFallbackChains(map[string][]string{"other": {"x"}})
	if got := r.FallbackChainFor("big"); got != nil {
		t.Errorf("replaced map still returns old chain: %v", got)
	}
}

func TestSetLocalDegradationChains_AppliesLive(t *testing.T) {
	r := newChainRouter(t, config.RoutingConfig{})
	r.SetLocalDegradationChains(map[string][]string{"big": {"small"}})
	if got := r.LocalDegradationChainFor("big"); len(got) != 1 || got[0] != "small" {
		t.Fatalf("after set = %v, want [small]", got)
	}
}

func TestChains_EmptyAndNilClear(t *testing.T) {
	r := newChainRouter(t, config.RoutingConfig{
		FallbackChains:         map[string][]string{"a": {"b"}},
		LocalDegradationChains: map[string][]string{"a": {"b"}},
	})
	r.SetFallbackChains(nil)
	r.SetLocalDegradationChains(map[string][]string{})
	if got := r.FallbackChainFor("a"); got != nil {
		t.Errorf("nil did not clear fallback chains: %v", got)
	}
	if got := r.LocalDegradationChainFor("a"); got != nil {
		t.Errorf("empty did not clear degradation chains: %v", got)
	}
}

func TestChains_AreIndependent(t *testing.T) {
	r := newChainRouter(t, config.RoutingConfig{})
	r.SetFallbackChains(map[string][]string{"a": {"b"}})
	if got := r.LocalDegradationChainFor("a"); got != nil {
		t.Errorf("setting fallback chains leaked into degradation chains: %v", got)
	}
	r.SetLocalDegradationChains(map[string][]string{"c": {"d"}})
	if got := r.FallbackChainFor("c"); got != nil {
		t.Errorf("setting degradation chains leaked into fallback chains: %v", got)
	}
	if got := r.FallbackChainFor("a"); len(got) != 1 {
		t.Errorf("fallback chain lost after degradation set: %v", got)
	}
}

func TestChains_InputMutationDoesNotLeak(t *testing.T) {
	in := map[string][]string{"a": {"b", "c"}}
	r := newChainRouter(t, config.RoutingConfig{})
	r.SetFallbackChains(in)
	r.SetLocalDegradationChains(in)
	in["a"][0] = "mutated"
	in["z"] = []string{"new"}
	for name, got := range map[string][]string{
		"fallback":    r.FallbackChainFor("a"),
		"degradation": r.LocalDegradationChainFor("a"),
	} {
		if len(got) != 2 || got[0] != "b" {
			t.Errorf("%s chain changed after caller mutation: %v", name, got)
		}
	}
	if r.FallbackChainFor("z") != nil || r.LocalDegradationChainFor("z") != nil {
		t.Error("key added to caller map after Set is visible")
	}
}

func TestNew_CopiesConfigChains(t *testing.T) {
	cfg := config.RoutingConfig{
		FallbackChains:         map[string][]string{"a": {"b"}},
		LocalDegradationChains: map[string][]string{"a": {"c"}},
	}
	r := newChainRouter(t, cfg)
	cfg.FallbackChains["a"][0] = "mutated"
	cfg.LocalDegradationChains["x"] = []string{"y"}
	if got := r.FallbackChainFor("a"); len(got) != 1 || got[0] != "b" {
		t.Errorf("fallback chain follows config mutation: %v", got)
	}
	if got := r.LocalDegradationChainFor("a"); len(got) != 1 || got[0] != "c" {
		t.Errorf("degradation chain follows config mutation: %v", got)
	}
	if r.LocalDegradationChainFor("x") != nil {
		t.Error("key added to config map after New is visible")
	}
}

func TestChains_ZeroValueRouterDoesNotPanic(t *testing.T) {
	r := &Router{}
	if r.FallbackChainFor("a") != nil || r.LocalDegradationChainFor("a") != nil {
		t.Error("zero-value router returned a chain")
	}
}

func TestChains_ConcurrentReadersAndWriters(t *testing.T) {
	r := newChainRouter(t, config.RoutingConfig{})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = r.FallbackChainFor("a")
					_ = r.LocalDegradationChainFor("a")
				}
			}
		}()
	}
	for i := 0; i < 500; i++ {
		r.SetFallbackChains(map[string][]string{"a": {"b"}})
		r.SetLocalDegradationChains(map[string][]string{"a": {"c"}})
		r.SetFallbackChains(nil)
		r.SetLocalDegradationChains(nil)
	}
	close(stop)
	wg.Wait()
}
