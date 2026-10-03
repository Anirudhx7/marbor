package admin

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/router"
)

// paramsInName extracts the parameter count (in billions) from a Hugging Face
// repo id such as "Qwen/Qwen2.5-72B-Instruct" or "...-13b-Instruct-hf". The
// second result is false when the name carries no size (e.g. "phi-4").
var paramsInName = regexp.MustCompile(`(?i)[-_](\d+(?:\.\d+)?)b(?:[-_.]|$)`)

func billionsInName(repo string) (float64, bool) {
	m := paramsInName.FindStringSubmatch(repo)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

func TestCatalogHFRepos_WellFormed(t *testing.T) {
	known := map[string]bool{}
	for _, cm := range catalogModels {
		known[cm.Name] = true
	}
	seenRepo := map[string]string{}
	for name, e := range catalogHFRepos {
		if !known[name] {
			t.Errorf("mapped name %q is not in catalogModels", name)
		}
		if _, dup := catalogHFUnmapped[name]; dup {
			t.Errorf("%q is in both catalogHFRepos and catalogHFUnmapped", name)
		}
		if !validHFRepoID.MatchString(e.Repo) {
			t.Errorf("%q: repo %q is not a valid org/name id", name, e.Repo)
		}
		if other, dup := seenRepo[e.Repo]; dup {
			t.Errorf("repo %q mapped by both %q and %q", e.Repo, other, name)
		}
		seenRepo[e.Repo] = name
		if e.Quant != "BF16" && e.Quant != "FP16" {
			t.Errorf("%q: quant %q, want BF16 or FP16 (full precision only)", name, e.Quant)
		}
		if e.SizeMB < 0 {
			t.Errorf("%q: negative size %d", name, e.SizeMB)
		}
		low := strings.ToLower(e.Repo)
		if strings.HasPrefix(low, "meta-llama/") || strings.HasPrefix(low, "google/") || strings.HasPrefix(low, "mistralai/") {
			t.Errorf("%q: repo %q is from an org whose models are gated", name, e.Repo)
		}
		// BF16/FP16 stores 2 bytes per parameter, so the weights are about
		// 1.86 GiB per billion parameters. Allow 1.7x to 2.4x GiB per billion
		// (rounding in the name, embeddings, tied or extra tensors). Only
		// checked where the parameter count is in the repo name.
		if b, ok := billionsInName(e.Repo); ok && e.SizeMB > 0 {
			lo, hi := b*1.7*1024, b*2.4*1024
			if float64(e.SizeMB) < lo || float64(e.SizeMB) > hi {
				t.Errorf("%q: size %d MiB is implausible for %.1fB parameters at 2 bytes each (want %.0f-%.0f MiB)", name, e.SizeMB, b, lo, hi)
			}
		}
	}
	for name, reason := range catalogHFUnmapped {
		if !known[name] {
			t.Errorf("unmapped name %q is not in catalogModels", name)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("unmapped %q has an empty reason", name)
		}
	}
	for _, cm := range catalogModels {
		_, mapped := catalogHFRepos[cm.Name]
		_, unmapped := catalogHFUnmapped[cm.Name]
		if mapped == unmapped {
			t.Errorf("catalog model %q must be in exactly one of mapped/unmapped (mapped=%v unmapped=%v)", cm.Name, mapped, unmapped)
		}
	}
}

// TestCatalogHFRepos_Live checks every mapped repo against the real Hugging
// Face API. Skipped unless MARBOR_HF_LIVE=1; never part of CI. Run it with -v
// and copy the printed sizes into catalogHFRepos when SizeMB is 0.
func TestCatalogHFRepos_Live(t *testing.T) {
	if os.Getenv("MARBOR_HF_LIVE") != "1" {
		t.Skip("set MARBOR_HF_LIVE=1 to check the table against huggingface.co")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	names := make([]string, 0, len(catalogHFRepos))
	for n := range catalogHFRepos {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		e := catalogHFRepos[name]
		t.Run(name, func(t *testing.T) {
			resp, err := client.Get("https://huggingface.co/api/models/" + e.Repo + "?blobs=true")
			if err != nil {
				t.Fatalf("fetch %s: %v", e.Repo, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status %d (move %q to catalogHFUnmapped)", e.Repo, resp.StatusCode, name)
			}
			var info struct {
				Gated    json.RawMessage `json:"gated"`
				Siblings []struct {
					Rfilename string `json:"rfilename"`
					Size      int64  `json:"size"`
				} `json:"siblings"`
				Safetensors struct {
					Parameters map[string]int64 `json:"parameters"`
				} `json:"safetensors"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
				t.Fatalf("decode %s: %v", e.Repo, err)
			}
			// HF reports false, or a string ("auto"/"manual") when gated.
			if strings.TrimSpace(string(info.Gated)) != "false" {
				t.Errorf("%s gated=%s (move %q to catalogHFUnmapped as gated)", e.Repo, info.Gated, name)
			}
			var total int64
			for _, s := range info.Siblings {
				if strings.HasSuffix(strings.ToLower(s.Rfilename), ".safetensors") {
					total += s.Size
				}
			}
			if total == 0 {
				t.Fatalf("%s has no .safetensors files", e.Repo)
			}
			liveMiB := total / (1024 * 1024)
			var dtype string
			var most int64
			for k, v := range info.Safetensors.Parameters {
				if v > most {
					dtype, most = k, v
				}
			}
			fmt.Printf("LIVE name=%s repo=%s gated=%s size_mib=%d dtype=%s\n", name, e.Repo, info.Gated, liveMiB, dtype)
			if dtype != "" && dtype != e.Quant {
				t.Errorf("%s stores %s weights, table says %s", e.Repo, dtype, e.Quant)
			}
			if e.SizeMB > 0 && math.Abs(float64(liveMiB-e.SizeMB))/float64(liveMiB) > 0.03 {
				t.Errorf("%s: table size %d MiB differs from live %d MiB by more than 3%%", e.Repo, e.SizeMB, liveMiB)
			}
		})
	}
}

// hfNodeEntry serves the catalog for a one-node fleet running the given
// runtime with the given declared VRAM, with Hugging Face access stubbed to
// fail the test: the catalog list must never leave the process.
func hfNodeEntry(t *testing.T, runtime string, vramMB int64) catalogNodeEntry {
	t.Helper()
	orig := hfHTTPClient.Transport
	hfHTTPClient.Transport = failingRoundTripper{t: t}
	defer func() { hfHTTPClient.Transport = orig }()
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := singleNodeServer(t, ollama.URL, runtime, func(n *router.NodeState) {
		n.VRAMTotalMB = vramMB
		n.VRAMSource = "declared"
	})
	return catalogFor(t, s).Nodes[0]
}

type failingRoundTripper struct{ t *testing.T }

func (f failingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("catalog handler made an outbound request to %s", r.URL)
	return nil, fmt.Errorf("outbound request blocked")
}

func modelFor(t *testing.T, n catalogNodeEntry, name string) catalogModelFit {
	t.Helper()
	for _, m := range n.Models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("model %q not in node entry", name)
	return catalogModelFit{}
}

func TestHandleModelCatalog_VLLMRowsFromTable(t *testing.T) {
	n := hfNodeEntry(t, "vllm", 24*1024)

	m := modelFor(t, n, "qwen2.5:7b")
	e := catalogHFRepos["qwen2.5:7b"]
	if len(m.Variants) != 1 || m.Variants[0].Tag != e.Repo || m.Variants[0].Quantization != e.Quant || m.Variants[0].SizeMB != e.SizeMB {
		t.Fatalf("qwen2.5:7b variants = %+v, want the single table row %+v", m.Variants, e)
	}
	if got := m.Recommendation; !got.Picked || got.Tag != e.Repo || got.Fit != "green" || got.Tight || got.SizeMB != e.SizeMB {
		t.Errorf("7B recommendation = %+v, want picked green %s", got, e.Repo)
	}

	// 14B BF16 is ~28 GB of weights: red on one 24 GB GPU, with the repo as the closest tag.
	big := modelFor(t, n, "qwen2.5:14b").Recommendation
	if big.Picked || big.Reason != reasonTooLarge || big.ClosestTag != catalogHFRepos["qwen2.5:14b"].Repo {
		t.Errorf("14B on 24 GB = %+v, want too_large with the repo as closest", big)
	}

	// Gated families stay "another format" even on a node with room.
	if got := modelFor(t, n, "llama3.1:8b").Recommendation; got.Picked || got.Reason != reasonIncompatibleRuntime {
		t.Errorf("unmapped llama3.1:8b = %+v, want incompatible_runtime", got)
	}
	for _, v := range modelFor(t, n, "llama3.1:8b").Variants {
		if v.Fit != "incompatible" {
			t.Errorf("unmapped variant %q fit = %q, want incompatible", v.Tag, v.Fit)
		}
	}
}

func TestHandleModelCatalog_VLLMTightAndOversized(t *testing.T) {
	n := hfNodeEntry(t, "vllm", 80*1024)
	// 32B BF16 is ~61 GiB of weights; with the safetensors overhead and the
	// list context it is above 85% of 80 GB but under 100%: picked, tight.
	got := modelFor(t, n, "qwen2.5-coder:32b").Recommendation
	if !got.Picked || !got.Tight || got.Fit != "yellow" {
		t.Errorf("32B on 80 GB = %+v, want picked tight yellow", got)
	}
	// 72B BF16 (~135 GiB) cannot fit one 80 GB GPU: vLLM/TGI size against the
	// largest single GPU.
	if got := modelFor(t, n, "qwen2.5:72b").Recommendation; got.Picked || got.Reason != reasonTooLarge {
		t.Errorf("72B on 80 GB = %+v, want too_large", got)
	}
	if got := modelFor(t, n, "deepseek-r1:70b").Recommendation; got.Picked || got.Reason != reasonTooLarge {
		t.Errorf("70B on 80 GB = %+v, want too_large", got)
	}
}

func TestHandleModelCatalog_VLLMSizeUnknown(t *testing.T) {
	orig := catalogHFRepos["qwen2.5:7b"]
	catalogHFRepos["qwen2.5:7b"] = catalogHFRepo{Repo: orig.Repo, Quant: orig.Quant}
	defer func() { catalogHFRepos["qwen2.5:7b"] = orig }()

	n := hfNodeEntry(t, "vllm", 80*1024)
	m := modelFor(t, n, "qwen2.5:7b")
	v := m.Variants[0]
	if v.SizeMB != 0 || v.VRAMEstMB != 0 || v.Fit != "unknown" {
		t.Errorf("size-unknown variant = size %d vram %d fit %q, want 0 / 0 / unknown (never a green 0)", v.SizeMB, v.VRAMEstMB, v.Fit)
	}
	if got := m.Recommendation; got.Picked || got.Reason != reasonVRAMUnknown {
		t.Errorf("size-unknown recommendation = %+v, want vram_unknown and no pick", got)
	}
}

func TestHandleModelCatalog_TGIRows(t *testing.T) {
	n := hfNodeEntry(t, "tgi", 24*1024)
	e := catalogHFRepos["phi3.5:3.8b"]
	if got := modelFor(t, n, "phi3.5:3.8b").Recommendation; !got.Picked || got.Tag != e.Repo {
		t.Errorf("phi3.5 on a tgi node = %+v, want picked %s", got, e.Repo)
	}
	if got := modelFor(t, n, "mistral:7b").Recommendation; got.Reason != reasonIncompatibleRuntime {
		t.Errorf("unmapped mistral on a tgi node = %+v, want incompatible_runtime", got)
	}
}

func TestHandleModelCatalog_LlamaCppAndMLXStayIncompatible(t *testing.T) {
	for _, rt := range []string{"llamacpp", "mlx"} {
		n := hfNodeEntry(t, rt, 80*1024)
		for _, m := range n.Models {
			if m.Recommendation.Picked || m.Recommendation.Reason != reasonIncompatibleRuntime {
				t.Errorf("%s: %q recommendation = %+v, want incompatible_runtime", rt, m.Name, m.Recommendation)
			}
		}
	}
}

func TestHandleModelCatalog_OllamaRowsUnchanged(t *testing.T) {
	n := hfNodeEntry(t, "ollama", 24*1024)
	m := modelFor(t, n, "qwen2.5:7b")
	if len(m.Variants) < 2 || m.Variants[0].Tag != "qwen2.5:7b" || m.Variants[0].Quantization != "Q4_K_M" {
		t.Errorf("ollama variants = %+v, want the compiled Ollama tags", m.Variants)
	}
}

func TestHandleModelCatalog_HFEstimateMatchesFeasibility(t *testing.T) {
	n := hfNodeEntry(t, "vllm", 80*1024)
	e := catalogHFRepos["qwen2.5:14b"]
	v := modelFor(t, n, "qwen2.5:14b").Variants[0]
	est, fit, _ := computeContextFeasibility(e.SizeMB, hfListContextTokens, safetensorsOverheadMult, safetensorsPerTokenMBFallback, n.VRAMTotalBytes, n.VRAMSource, nil, "")
	if v.VRAMEstMB != est/(1024*1024) || v.Fit != fit {
		t.Errorf("list row = vram %d fit %q, want %d %q from computeContextFeasibility", v.VRAMEstMB, v.Fit, est/(1024*1024), fit)
	}
}
