package router

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	runtimepkg "github.com/Anirudhx7/marbor/internal/runtime"
)

func TestModelInfoFromProbe_GPUPercent(t *testing.T) {
	cases := []struct {
		name       string
		size, vram int64
		want       *int
	}{
		{"partial", 100, 62, intp(62)},
		{"full", 100, 100, intp(100)},
		{"floored", 1000, 996, intp(99)},
		{"tiny share is at least 1", 1000, 1, intp(1)},
		{"zero vram with size is a real 0", 100, 0, intp(0)},
		{"unknown size", 0, 62, nil},
		{"negative size", -5, 62, nil},
		{"negative vram", 100, -1, nil},
		{"vram above size clamps", 100, 150, intp(100)},
		{"huge values do not overflow", 1 << 62, 1 << 61, intp(50)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := modelInfoFromProbe(runtimepkg.LoadedModel{Name: "m", SizeBytes: c.size, SizeVRAMBytes: c.vram})
			switch {
			case c.want == nil && m.GPUPercent != nil:
				t.Fatalf("want nil, got %d", *m.GPUPercent)
			case c.want != nil && m.GPUPercent == nil:
				t.Fatalf("want %d, got nil", *c.want)
			case c.want != nil && *m.GPUPercent != *c.want:
				t.Fatalf("want %d, got %d", *c.want, *m.GPUPercent)
			}
		})
	}
}

func TestModelInfoFromProbe_ContextLength(t *testing.T) {
	if m := modelInfoFromProbe(runtimepkg.LoadedModel{Name: "m"}); m.ContextLength != nil {
		t.Errorf("unreported context must be nil, got %d", *m.ContextLength)
	}
	m := modelInfoFromProbe(runtimepkg.LoadedModel{Name: "m", ContextLength: 4096})
	if m.ContextLength == nil || *m.ContextLength != 4096 {
		t.Errorf("context = %v, want 4096", m.ContextLength)
	}
}

func TestModelInfoJSON_OmitsUnknown(t *testing.T) {
	b, _ := json.Marshal(ModelInfo{Name: "a"})
	if strings.Contains(string(b), "gpuPercent") || strings.Contains(string(b), "contextLength") {
		t.Errorf("unknown values must be absent: %s", b)
	}
	b, _ = json.Marshal(modelInfoFromProbe(runtimepkg.LoadedModel{Name: "a", SizeBytes: 10, SizeVRAMBytes: 0, ContextLength: 2048}))
	if !strings.Contains(string(b), `"gpuPercent":0`) || !strings.Contains(string(b), `"contextLength":2048`) {
		t.Errorf("a real 0 and a context must be present: %s", b)
	}
}

func TestAttributeSoleModelVRAM_KeepsOffloadFields(t *testing.T) {
	g, c := 62, int64(4096)
	n := &NodeState{
		LoadedModels: []ModelInfo{{Name: "m", SizeVRAM: 0, GPUPercent: &g, ContextLength: &c}},
		VRAMUsedMB:   1000,
	}
	attributeSoleModelVRAM(n)
	got := n.LoadedModels[0]
	if got.GPUPercent == nil || *got.GPUPercent != 62 || got.ContextLength == nil || *got.ContextLength != 4096 {
		t.Errorf("attribution dropped offload fields: %+v", got)
	}
}

func TestAttributeSoleModelVRAM_NonOllamaStaysNil(t *testing.T) {
	n := &NodeState{LoadedModels: []ModelInfo{{Name: "m"}}, VRAMUsedMB: 1000}
	attributeSoleModelVRAM(n)
	if n.LoadedModels[0].GPUPercent != nil || n.LoadedModels[0].ContextLength != nil {
		t.Errorf("attributed VRAM must never create a GPU share: %+v", n.LoadedModels[0])
	}
}

type seqProbe struct {
	results []runtimepkg.ProbeResult
	i       int
}

func (p *seqProbe) Probe(ctx context.Context, nodeURL string) (runtimepkg.ProbeResult, error) {
	r := p.results[p.i]
	if p.i < len(p.results)-1 {
		p.i++
	}
	return r, nil
}

// A poll replaces the loaded-model list wholesale, so a stale share or context
// never survives into the next reading, and a model with no reading is nil.
func TestPollNode_OffloadFieldsReplacedEachPoll(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Runtime: "ollama"},
	}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.probe = &seqProbe{results: []runtimepkg.ProbeResult{
		{LoadedModels: []runtimepkg.LoadedModel{{Name: "m", SizeBytes: 100, SizeVRAMBytes: 62, ContextLength: 4096}}},
		{LoadedModels: []runtimepkg.LoadedModel{{Name: "m", SizeVRAMBytes: 62}}},
	}}
	n.mu.Unlock()

	r.pollNode(n)
	n.mu.RLock()
	first := n.LoadedModels[0]
	n.mu.RUnlock()
	if first.GPUPercent == nil || *first.GPUPercent != 62 || first.ContextLength == nil || *first.ContextLength != 4096 {
		t.Fatalf("first poll: %+v", first)
	}

	r.pollNode(n)
	n.mu.RLock()
	second := n.LoadedModels[0]
	n.mu.RUnlock()
	if second.GPUPercent != nil || second.ContextLength != nil {
		t.Errorf("second poll must not keep the old values: %+v", second)
	}
}
