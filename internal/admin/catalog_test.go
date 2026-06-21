package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// catalogResponse mirrors the JSON shape returned by handleModelCatalog.
type catalogResponse struct {
	Catalog []CatalogModel     `json:"catalog"`
	Nodes   []catalogNodeEntry `json:"nodes"`
}

func TestCatalogHasAtLeast25Models(t *testing.T) {
	if len(catalogModels) < 25 {
		t.Fatalf("catalog has %d models, want >= 25", len(catalogModels))
	}
	// Every model needs at least one variant and a recommended one.
	for _, m := range catalogModels {
		if m.Name == "" || m.DisplayName == "" {
			t.Errorf("model %q missing name/display_name", m.Name)
		}
		if len(m.Variants) == 0 {
			t.Errorf("model %q has no variants", m.Name)
		}
		if len(m.Categories) == 0 {
			t.Errorf("model %q has no categories", m.Name)
		}
		hasRec := false
		for _, v := range m.Variants {
			if v.VRAMEstMB <= 0 {
				t.Errorf("model %q variant %q has non-positive VRAM estimate", m.Name, v.Tag)
			}
			if v.Recommended {
				hasRec = true
			}
		}
		if !hasRec {
			t.Errorf("model %q has no recommended variant", m.Name)
		}
	}
}

func TestHandleModelCatalog_HappyPath(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp catalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Catalog) < 25 {
		t.Errorf("catalog returned %d models, want >= 25", len(resp.Catalog))
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(resp.Nodes))
	}

	node := resp.Nodes[0]
	if node.VRAMSource != "nvidia-smi" {
		t.Errorf("vram_source = %q, want nvidia-smi", node.VRAMSource)
	}
	// Node has 8 GB total, 4 GB loaded by llama3:8b -> ~4 GB free.
	if len(node.Models) != len(catalogModels) {
		t.Fatalf("node has %d model entries, want %d", len(node.Models), len(catalogModels))
	}

	// Find a small model (llama3.2:1b, ~1GB) -> should be green. A 70B model
	// (~40GB) -> red. Verify classification ran against real free VRAM.
	fitByName := map[string]string{}
	for _, m := range node.Models {
		// Use the recommended variant's fit.
		for _, v := range m.Variants {
			if v.Recommended {
				fitByName[m.Name] = v.Fit
			}
		}
	}
	if got := fitByName["llama3.2:1b"]; got != "green" {
		t.Errorf("llama3.2:1b fit = %q, want green (fits in ~4GB free)", got)
	}
	if got := fitByName["llama3.1:70b"]; got != "red" {
		t.Errorf("llama3.1:70b fit = %q, want red (needs 40GB)", got)
	}
}

func TestHandleModelCatalog_Downloaded(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	var resp catalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// mockOllamaServer reports llama3:8b downloaded. The catalog has no
	// "llama3:8b" entry, but cross-referencing must not panic and embedding
	// models (not downloaded) should be flagged false.
	for _, m := range resp.Nodes[0].Models {
		if m.Name == "nomic-embed-text" && m.Downloaded {
			t.Errorf("nomic-embed-text incorrectly flagged downloaded")
		}
	}
}

func TestHandleModelCatalog_V1Route(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/models/catalog", nil)
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("v1 route status = %d, want 200", w.Code)
	}
}

func TestHandleModelCatalog_Unauthorized(t *testing.T) {
	s := newModelFitTestServer("http://127.0.0.1:1")

	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestClassifyFit(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	cases := []struct {
		name      string
		estBytes  int64
		freeBytes int64
		source    string
		want      string
	}{
		{"comfortable", 2 * gb, 8 * gb, "nvidia-smi", "green"},
		{"tight", 7 * gb, 8 * gb, "nvidia-smi", "yellow"},
		{"too-large", 40 * gb, 8 * gb, "nvidia-smi", "red"},
		{"unknown-source", 2 * gb, 0, "unknown", "unknown"},
		{"inferred-source", 2 * gb, 4 * gb, "inferred", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyFit(c.estBytes, c.freeBytes, c.source); got != c.want {
				t.Errorf("classifyFit = %q, want %q", got, c.want)
			}
		})
	}
}

func TestExtractQuantization(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		{"Llama-3.2-1B-Instruct-Q4_K_M.gguf", "Q4_K_M"},
		{"model-q8_0.gguf", "Q8_0"},
		{"phi3-fp16.gguf", "FP16"},
		{"custom_model-IQ4_XS.gguf", "IQ4_XS"},
		{"llama-3-8b.gguf", "GGUF"},
		{"some-other-quant-format-bf16.gguf", "BF16"},
		{"no_hyphen_q4_k_m.gguf", "Q4_K_M"},
		{"q4_k_m-in-middle-but-ends-with-something.gguf", "Q4_K_M"},
	}

	for _, c := range cases {
		if got := extractQuantization(c.filename); got != c.want {
			t.Errorf("extractQuantization(%q) = %q, want %q", c.filename, got, c.want)
		}
	}
}
