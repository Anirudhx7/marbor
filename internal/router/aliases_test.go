package router

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

func TestResolveModelAlias_SingleHop(t *testing.T) {
	r := New(config.RoutingConfig{ModelAliases: map[string]string{"gpt-4": "llama3.2:8b"}}, nil, nil)

	if got, ok := r.ResolveModelAlias("gpt-4"); !ok || got != "llama3.2:8b" {
		t.Fatalf("ResolveModelAlias(gpt-4) = %q, %v; want llama3.2:8b, true", got, ok)
	}
	// The target itself is not an alias: resolving it is a no-op.
	if got, ok := r.ResolveModelAlias("llama3.2:8b"); ok {
		t.Fatalf("ResolveModelAlias(target) = %q, true; want no match", got)
	}
	if _, ok := r.ResolveModelAlias(""); ok {
		t.Fatal("empty name must never resolve")
	}

	// Caller's map is copied, not retained.
	in := map[string]string{"a": "b"}
	r.SetModelAliases(in)
	in["a"] = "mutated"
	if got, _ := r.ResolveModelAlias("a"); got != "b" {
		t.Fatalf("SetModelAliases must copy its input; got %q", got)
	}
	// Returned copy is independent too.
	cp := r.ModelAliases()
	cp["a"] = "mutated"
	if got, _ := r.ResolveModelAlias("a"); got != "b" {
		t.Fatalf("ModelAliases must return a copy; got %q", got)
	}

	r.SetModelAliases(nil)
	if _, ok := r.ResolveModelAlias("a"); ok {
		t.Fatal("SetModelAliases(nil) must clear every alias")
	}
	if len(r.ModelAliases()) != 0 {
		t.Fatal("ModelAliases() after clear must be empty")
	}
}

func TestSetModelAliases_ConcurrentResolve_Race(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
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
				}
				if got, ok := r.ResolveModelAlias("gpt-4"); ok && got == "" {
					t.Error("resolved to empty target")
					return
				}
				_ = r.ModelAliases()
			}
		}()
	}
	for i := 0; i < 500; i++ {
		r.SetModelAliases(map[string]string{"gpt-4": fmt.Sprintf("model-%d", i)})
		if i%7 == 0 {
			r.SetModelAliases(nil)
		}
	}
	close(stop)
	wg.Wait()
}
