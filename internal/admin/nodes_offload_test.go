package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/router"
)

func TestNodesJSON_LoadedModelOffloadKeys(t *testing.T) {
	gpu, ctxLen := 62, int64(4096)
	zero := 0
	resp := nodeResp{Health: "healthy", LoadedModels: []router.ModelInfo{
		{Name: "partial", GPUPercent: &gpu, ContextLength: &ctxLen},
		{Name: "cpu-only", GPUPercent: &zero},
		{Name: "unreported"},
	}}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		LoadedModels []map[string]any `json:"loadedModels"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.LoadedModels) != 3 {
		t.Fatalf("got %d models: %s", len(out.LoadedModels), b)
	}
	if out.LoadedModels[0]["gpuPercent"] != float64(62) || out.LoadedModels[0]["contextLength"] != float64(4096) {
		t.Errorf("set values missing: %v", out.LoadedModels[0])
	}
	if v, ok := out.LoadedModels[1]["gpuPercent"]; !ok || v != float64(0) {
		t.Errorf("a real 0 must be present: %v", out.LoadedModels[1])
	}
	for k := range out.LoadedModels[2] {
		if strings.EqualFold(k, "gpuPercent") || strings.EqualFold(k, "contextLength") {
			t.Errorf("unknown values must be omitted, found %q", k)
		}
	}
}
