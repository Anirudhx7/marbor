package main

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// patchOnlyFields lists router.NodePatch fields that are deliberately not
// persisted in store.NodeOverride. Growing this set needs a deliberate edit
// here, with a reason.
var patchOnlyFields = map[string]string{
	"URL": "applied via UpdateNodeURL, never persisted as a node override",
}

// nodeProj is the projection of NodeState that the boot reload is responsible
// for: one entry per persisted override field. It is a plain value so tests
// never copy a whole NodeState (it holds a mutex). Derived side effects of an
// override (VRAMSource alongside VRAMTotalMB, the runtime mismatch hint, the
// probe reset alongside Runtime) are deliberately not projected here.
type nodeProj struct {
	VRAMTotalMBConfig  int64
	GPUModel           string
	Runtime            string
	DeclaredGPUIndices []int
	MaxInFlight        int
	TLSFingerprint     string
	ParallelismType    string
	ParallelismWidth   int
	VRAMOverrides      map[string]int64
	ReplicaPeers       store.ReplicaPeers
	HasReplicaPeers    bool
}

// bootCase describes one persisted override field: how to seed it, and how
// the router state is expected to change once the boot reload applied it.
// Seeds differ from the router.New defaults used by newBootRouter, so an
// unapplied field cannot pass by accident.
type bootCase struct {
	seed func(*store.NodeOverride)
	want func(*nodeProj)
}

var bootReloadCases = map[string]bootCase{
	"VRAMTotalMB": {
		seed: func(o *store.NodeOverride) { v := int64(81920); o.VRAMTotalMB = &v },
		want: func(p *nodeProj) { p.VRAMTotalMBConfig = 81920 },
	},
	"GPUModel": {
		seed: func(o *store.NodeOverride) { v := "H100"; o.GPUModel = &v },
		want: func(p *nodeProj) { p.GPUModel = "H100" },
	},
	"Runtime": {
		seed: func(o *store.NodeOverride) { v := "vllm"; o.Runtime = &v },
		want: func(p *nodeProj) { p.Runtime = "vllm" },
	},
	"GPUIndices": {
		seed: func(o *store.NodeOverride) { v := []int{2, 3}; o.GPUIndices = &v },
		want: func(p *nodeProj) { p.DeclaredGPUIndices = []int{2, 3} },
	},
	"MaxInFlight": {
		seed: func(o *store.NodeOverride) { v := 7; o.MaxInFlight = &v },
		want: func(p *nodeProj) { p.MaxInFlight = 7 },
	},
	"TLSFingerprint": {
		seed: func(o *store.NodeOverride) { v := "SHA256:AAAABBBBCCCC"; o.TLSFingerprint = &v },
		want: func(p *nodeProj) { p.TLSFingerprint = "SHA256:AAAABBBBCCCC" },
	},
	"ParallelismType": {
		seed: func(o *store.NodeOverride) { v := "tp"; o.ParallelismType = &v },
		want: func(p *nodeProj) { p.ParallelismType = "tp" },
	},
	"ParallelismWidth": {
		seed: func(o *store.NodeOverride) { v := 4; o.ParallelismWidth = &v },
		want: func(p *nodeProj) { p.ParallelismWidth = 4 },
	},
	"VRAMOverrides": {
		seed: func(o *store.NodeOverride) { v := map[string]int64{"llama3:70b": 40000}; o.VRAMOverrides = &v },
		want: func(p *nodeProj) { p.VRAMOverrides = map[string]int64{"llama3:70b": 40000} },
	},
	"ReplicaPeers": {
		seed: func(o *store.NodeOverride) {
			o.ReplicaPeers = &store.ReplicaPeers{Members: []string{"node-a", "node-b"}, Head: "node-a"}
		},
		want: func(p *nodeProj) {
			p.ReplicaPeers = store.ReplicaPeers{Members: []string{"node-a", "node-b"}, Head: "node-a"}
			p.HasReplicaPeers = true
		},
	},
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func structFieldNames(t reflect.Type) map[string]bool {
	out := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out[t.Field(i).Name] = true
	}
	return out
}

// TestNodeOverrideSchemaMatchesBootReloadCases fails when store.NodeOverride,
// router.NodePatch and the boot reload cases drift apart: a persisted field
// with no case here is a field nobody proved is applied at boot.
func TestNodeOverrideSchemaMatchesBootReloadCases(t *testing.T) {
	overrideFields := structFieldNames(reflect.TypeOf(store.NodeOverride{}))
	patchFields := structFieldNames(reflect.TypeOf(router.NodePatch{}))

	for name := range overrideFields {
		c, ok := bootReloadCases[name]
		if !ok {
			t.Errorf("store.NodeOverride.%s has no boot-reload case - add one AND wire it in applyNodeOverrides", name)
			continue
		}
		if c.seed == nil || c.want == nil {
			t.Errorf("boot-reload case %s must set both seed and want", name)
		}
		if !patchFields[name] {
			t.Errorf("store.NodeOverride.%s has no matching router.NodePatch field", name)
		}
	}
	for name := range bootReloadCases {
		if !overrideFields[name] {
			t.Errorf("boot-reload case %s has no store.NodeOverride field", name)
		}
	}
	for name := range patchFields {
		if _, persisted := bootReloadCases[name]; persisted {
			continue
		}
		if _, ok := patchOnlyFields[name]; !ok {
			t.Errorf("router.NodePatch.%s is neither persisted nor listed in patchOnlyFields", name)
		}
	}
	if len(patchOnlyFields) != 1 {
		t.Errorf("patchOnlyFields = %v, want exactly URL", patchOnlyFields)
	}
	for name, reason := range patchOnlyFields {
		if name != "URL" || reason == "" {
			t.Errorf("patchOnlyFields entry %q (reason %q) needs a deliberate review", name, reason)
		}
	}
}

// newBootRouter builds a router with two nodes via router.New only (never
// AddNode, which starts a poll goroutine that would race the test's reads).
func newBootRouter() *router.Router {
	cfg := config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 100}
	nodes := []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434", Runtime: "ollama", VRAMTotalMB: 24000, GPUModel: "RTX 3090", MaxInFlight: 3},
		{Name: "node-b", URL: "http://localhost:11435", Runtime: "ollama", VRAMTotalMB: 24000, GPUModel: "RTX 3090", MaxInFlight: 3},
	}
	return router.New(cfg, nodes, nil)
}

func projectNode(t *testing.T, r *router.Router, name string) nodeProj {
	t.Helper()
	for _, n := range r.Nodes() {
		if n.Name != name {
			continue
		}
		var p nodeProj
		// No lock needed: router.New starts no goroutine and the router is never started.
		p = nodeProj{
			VRAMTotalMBConfig:  n.VRAMTotalMBConfig,
			GPUModel:           n.GPUModel,
			Runtime:            n.Runtime,
			DeclaredGPUIndices: append([]int(nil), n.DeclaredGPUIndices...),
			MaxInFlight:        n.MaxInFlight,
			TLSFingerprint:     n.TLSFingerprint,
			ParallelismType:    n.ParallelismType,
			ParallelismWidth:   n.ParallelismWidth,
		}
		if n.VRAMOverrides != nil {
			p.VRAMOverrides = make(map[string]int64, len(n.VRAMOverrides))
			for k, v := range n.VRAMOverrides {
				p.VRAMOverrides[k] = v
			}
		}
		if n.ReplicaPeers != nil {
			p.HasReplicaPeers = true
			p.ReplicaPeers = *n.ReplicaPeers
			p.ReplicaPeers.Members = append([]string(nil), n.ReplicaPeers.Members...)
		}
		return p
	}
	t.Fatalf("node %q not found in router", name)
	return nodeProj{}
}

// persistAndReload writes one override through the positional store API (the
// only call site of it in this file), closes the store, reopens it as a
// restarted process would, and returns what the reopened store reads back.
// The read-back must equal the input, so a field this helper forgets to pass
// fails by name here rather than as a silent missing seed.
func persistAndReload(t *testing.T, name string, ov store.NodeOverride) map[string]store.NodeOverride {
	t.Helper()
	path := filepath.Join(t.TempDir(), "marbor.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	firstClosed := false
	defer func() {
		if !firstClosed {
			_ = st.Close()
		}
	}()
	if err := st.UpsertNodeOverride(name, ov.VRAMTotalMB, ov.GPUModel, ov.Runtime, ov.GPUIndices, ov.MaxInFlight,
		ov.TLSFingerprint, ov.ParallelismType, ov.ParallelismWidth, ov.VRAMOverrides, ov.ReplicaPeers); err != nil {
		t.Fatalf("UpsertNodeOverride: %v", err)
	}
	firstClosed = true
	if err := st.Close(); err != nil {
		t.Fatalf("first store Close: %v", err)
	}
	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open (reopen): %v", err)
	}
	t.Cleanup(func() {
		if err := st2.Close(); err != nil {
			t.Errorf("reopened store Close: %v", err)
		}
	})
	got, err := st2.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	if !reflect.DeepEqual(got[name], ov) {
		t.Fatalf("store roundtrip changed the override:\n got  %+v\n want %+v", got[name], ov)
	}
	return got
}

// TestBootReloadAppliesEveryPersistedOverride seeds every field at once,
// restarts the store, and checks the router ends up with all of them.
func TestBootReloadAppliesEveryPersistedOverride(t *testing.T) {
	var ov store.NodeOverride
	r := newBootRouter()
	want := projectNode(t, r, "node-a")
	for _, name := range sortedKeys(bootReloadCases) {
		bootReloadCases[name].seed(&ov)
		bootReloadCases[name].want(&want)
	}

	applyNodeOverrides(r, persistAndReload(t, "node-a", ov))

	if got := projectNode(t, r, "node-a"); !reflect.DeepEqual(got, want) {
		t.Errorf("node-a after boot reload:\n got  %+v\n want %+v", got, want)
	}
	if got, base := projectNode(t, r, "node-b"), projectNode(t, newBootRouter(), "node-b"); !reflect.DeepEqual(got, base) {
		t.Errorf("node-b has no override and must stay at its baseline:\n got  %+v\n want %+v", got, base)
	}
}

// TestBootReloadPerFieldIsolation seeds ONE field at a time and requires that
// field to change while every other persisted field stays at baseline, which
// catches a field wired into the wrong patch slot.
func TestBootReloadPerFieldIsolation(t *testing.T) {
	for _, name := range sortedKeys(bootReloadCases) {
		c := bootReloadCases[name]
		t.Run(name, func(t *testing.T) {
			r := newBootRouter()
			baseline := projectNode(t, r, "node-a")
			want := baseline
			c.want(&want)
			if reflect.DeepEqual(want, baseline) {
				t.Fatalf("case %s expects no change from the baseline, so it would pass vacuously", name)
			}

			var ov store.NodeOverride
			c.seed(&ov)
			applyNodeOverrides(r, persistAndReload(t, "node-a", ov))

			if got := projectNode(t, r, "node-a"); !reflect.DeepEqual(got, want) {
				t.Errorf("only %s was persisted:\n got  %+v\n want %+v", name, got, want)
			}
		})
	}
}

func TestBootReloadEdgeCases(t *testing.T) {
	t.Run("unknown node and empty map do not panic", func(t *testing.T) {
		r := newBootRouter()
		base := projectNode(t, r, "node-a")
		applyNodeOverrides(r, nil)
		applyNodeOverrides(r, map[string]store.NodeOverride{})
		gpuModel := "H100"
		applyNodeOverrides(r, map[string]store.NodeOverride{
			"ghost":  {GPUModel: &gpuModel},
			"node-b": {GPUModel: &gpuModel},
		})
		if got := projectNode(t, r, "node-a"); !reflect.DeepEqual(got, base) {
			t.Errorf("node-a changed by an override for other nodes: %+v", got)
		}
		if got := projectNode(t, r, "node-b").GPUModel; got != "H100" {
			t.Errorf("node-b GPUModel = %q, want H100 (known node patched despite unknown sibling)", got)
		}
	})

	t.Run("non-nil empty values survive the restart", func(t *testing.T) {
		r := newBootRouter()
		seeded := []int{5}
		if !r.PatchNode("node-a", router.NodePatch{GPUIndices: &seeded}) {
			t.Fatal("PatchNode: node-a not found")
		}
		if got := projectNode(t, r, "node-a"); len(got.DeclaredGPUIndices) == 0 || got.VRAMOverrides != nil {
			t.Fatalf("baseline must have GPU indices and nil VRAMOverrides, got %+v", got)
		}
		noIndices, noModels := []int{}, map[string]int64{}
		applyNodeOverrides(r, persistAndReload(t, "node-a", store.NodeOverride{
			GPUIndices:    &noIndices,
			VRAMOverrides: &noModels,
			ReplicaPeers:  &store.ReplicaPeers{},
		}))
		got := projectNode(t, r, "node-a")
		if len(got.DeclaredGPUIndices) != 0 {
			t.Errorf("DeclaredGPUIndices = %v, want empty", got.DeclaredGPUIndices)
		}
		if got.VRAMOverrides == nil || len(got.VRAMOverrides) != 0 {
			t.Errorf("VRAMOverrides = %#v, want non-nil and empty", got.VRAMOverrides)
		}
		if !got.HasReplicaPeers || len(got.ReplicaPeers.Members) != 0 || got.ReplicaPeers.Head != "" {
			t.Errorf("ReplicaPeers = %+v (present=%v), want present and empty", got.ReplicaPeers, got.HasReplicaPeers)
		}
	})

	t.Run("every runtime reloads", func(t *testing.T) {
		for _, rt := range []string{"ollama", "vllm", "tgi", "llamacpp", "mlx"} {
			t.Run(rt, func(t *testing.T) {
				r := newBootRouter()
				// Start from a runtime different from rt so the reload is a real change.
				other := "vllm"
				if rt == "vllm" {
					other = "tgi"
				}
				if !r.PatchNode("node-a", router.NodePatch{Runtime: &other}) {
					t.Fatal("PatchNode: node-a not found")
				}
				applyNodeOverrides(r, persistAndReload(t, "node-a", store.NodeOverride{Runtime: &rt}))
				if got := projectNode(t, r, "node-a").Runtime; got != rt {
					t.Errorf("Runtime = %q after reload, want %q", got, rt)
				}
			})
		}
	})

	t.Run("legacy row with only the original three fields", func(t *testing.T) {
		r := newBootRouter()
		base := projectNode(t, r, "node-a")
		vram, model, rt := int64(81920), "H100", "vllm"
		applyNodeOverrides(r, persistAndReload(t, "node-a", store.NodeOverride{VRAMTotalMB: &vram, GPUModel: &model, Runtime: &rt}))

		want := base
		want.VRAMTotalMBConfig, want.GPUModel, want.Runtime = vram, model, rt
		if got := projectNode(t, r, "node-a"); !reflect.DeepEqual(got, want) {
			t.Errorf("legacy row:\n got  %+v\n want %+v", got, want)
		}
	})
}
