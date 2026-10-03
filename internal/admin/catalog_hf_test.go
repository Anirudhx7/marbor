package admin

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/marboragent"
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

// expectedBillions is the parameter count, in billions, of each mapped model.
// BF16/FP16 stores 2 bytes per parameter, so the weights are about 1.86 GiB per
// billion parameters; the size check allows 1.7x to 2.4x GiB per billion
// (rounding in the name, embeddings, tied or extra tensors).
var expectedBillions = map[string]float64{
	"qwen2.5:7b": 7, "qwen2.5:14b": 14, "qwen2.5:72b": 72,
	"qwen2.5-coder:7b": 7, "qwen2.5-coder:32b": 32,
	"deepseek-r1:7b": 7, "deepseek-r1:14b": 14, "deepseek-r1:32b": 32, "deepseek-r1:70b": 70,
	"phi4:14b": 14, "phi3.5:3.8b": 3.8,
	"codellama:7b": 7, "codellama:13b": 13,
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
		// The parameter count is typed per model here, independent of the repo
		// name, so repos with no size in the name (phi-4, Phi-3.5-mini) are
		// checked too.
		b, ok := expectedBillions[name]
		if !ok {
			t.Errorf("%q: add its parameter count (billions) to expectedBillions", name)
		}
		if nb, named := billionsInName(e.Repo); named && ok && math.Abs(nb-b)/b > 0.1 {
			t.Errorf("%q: repo name says %.1fB, expectedBillions says %.1fB", name, nb, b)
		}
		if e.SizeMB > 0 && ok {
			lo, hi := b*1.7*1024, b*2.4*1024
			if float64(e.SizeMB) < lo || float64(e.SizeMB) > hi {
				t.Errorf("%q: size %d MiB is implausible for %.1fB parameters at 2 bytes each (want %.0f-%.0f MiB)", name, e.SizeMB, b, lo, hi)
			}
		}
		low := strings.ToLower(e.Repo)
		if strings.HasPrefix(low, "meta-llama/") || strings.HasPrefix(low, "google/") || strings.HasPrefix(low, "mistralai/") {
			t.Errorf("%q: repo %q is from an org whose models are gated", name, e.Repo)
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
// and copy the printed sizes into catalogHFRepos when SizeMB is 0 (a 0 in a
// mapped entry fails here so it cannot ship unnoticed).
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
			if liveMiB == 0 {
				t.Fatalf("%s: safetensors total is under 1 MiB (%d bytes), not a real weights repo", e.Repo, total)
			}
			var dtype string
			var most int64
			for k, v := range info.Safetensors.Parameters {
				if v > most {
					dtype, most = k, v
				}
			}
			fmt.Printf("LIVE name=%s repo=%s gated=%s size_mib=%d dtype=%s\n", name, e.Repo, info.Gated, liveMiB, dtype)
			if dtype == "" {
				t.Errorf("%s: Hugging Face reports no safetensors dtype, cannot confirm table dtype %s", e.Repo, e.Quant)
			} else if dtype != e.Quant {
				t.Errorf("%s stores %s weights, table says %s", e.Repo, dtype, e.Quant)
			}
			if e.SizeMB <= 0 {
				t.Errorf("%s: table size is %d, set it to the live %d MiB", e.Repo, e.SizeMB, liveMiB)
			} else if math.Abs(float64(liveMiB-e.SizeMB))/float64(liveMiB) > 0.03 {
				t.Errorf("%s: table size %d MiB differs from live %d MiB by more than 3%%", e.Repo, e.SizeMB, liveMiB)
			}
		})
	}
}

// hfNodeEntryWith serves the catalog for a one-node fleet running the given
// runtime, with Hugging Face access stubbed to fail the test: the catalog list
// must never leave the process. Only hfHTTPClient is swapped: the process-wide
// default transport is left alone because background goroutines from other
// tests read it concurrently.
//
// It mutates process-global state (hfHTTPClient), so tests that call it must not
// run in parallel (no t.Parallel).
func hfNodeEntryWith(t *testing.T, runtime string, mutate func(n *router.NodeState)) catalogNodeEntry {
	t.Helper()
	blocked := &outboundRecorder{}
	origTransport := hfHTTPClient.Transport
	hfHTTPClient.Transport = failingRoundTripper{rec: blocked}
	defer func() {
		hfHTTPClient.Transport = origTransport
	}()
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := singleNodeServer(t, ollama.URL, runtime, func(n *router.NodeState) {
		if mutate != nil {
			mutate(n)
		}
	})
	entry := catalogFor(t, s).Nodes[0]
	if n := blocked.count.Load(); n > 0 {
		t.Errorf("catalog handler made %d outbound request(s), first to %v", n, blocked.first.Load())
	}
	return entry
}

// hfNodeEntry is hfNodeEntryWith for a node with the given declared VRAM.
func hfNodeEntry(t *testing.T, runtime string, vramMB int64) catalogNodeEntry {
	t.Helper()
	return hfNodeEntryWith(t, runtime, func(n *router.NodeState) {
		n.VRAMTotalMB = vramMB
		n.VRAMSource = "declared"
	})
}

// outboundRecorder records blocked outbound requests. The transport can be
// called from goroutines that outlive the test body, so it must not touch
// testing.T; the test reads the recorder after the handler returns.
type outboundRecorder struct {
	count atomic.Int32
	first atomic.Value // URL string of the first blocked request
}

// failingRoundTripper blocks and records any outbound request.
type failingRoundTripper struct {
	rec *outboundRecorder
}

func (f failingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Record the URL before bumping the count so a reader that sees count > 0
	// always finds first set.
	f.rec.first.CompareAndSwap(nil, r.URL.String())
	f.rec.count.Add(1)
	return nil, fmt.Errorf("outbound request blocked")
}

// withHFSize sets the table size of one mapped model for the test. It mutates
// the package-level catalogHFRepos, so tests that call it must not run in
// parallel (no t.Parallel).
func withHFSize(t *testing.T, name string, sizeMB int64) {
	t.Helper()
	orig := catalogHFRepos[name]
	e := orig
	e.SizeMB = sizeMB
	catalogHFRepos[name] = e
	t.Cleanup(func() { catalogHFRepos[name] = orig })
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
	if len(n.Models) != len(catalogModels) {
		t.Fatalf("node lists %d models, want all %d catalog models", len(n.Models), len(catalogModels))
	}

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
		if len(n.Models) == 0 {
			t.Fatalf("%s: node lists no models, the loop below would assert nothing", rt)
		}
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
	// Every model keeps its compiled variants with the Ollama estimate and the
	// 85% / 100% capacity bands of a 24 GB node, untouched by the Hugging Face rows.
	const capMB = 24 * 1024
	if len(n.Models) != len(catalogModels) {
		t.Fatalf("node lists %d models, want %d", len(n.Models), len(catalogModels))
	}
	for i, got := range n.Models {
		want := catalogModels[i]
		if got.Name != want.Name || len(got.Variants) != len(want.Variants) || len(got.Variants) == 0 {
			t.Fatalf("model %d = %s with %d variants, want %s with %d", i, got.Name, len(got.Variants), want.Name, len(want.Variants))
		}
		for j, v := range got.Variants {
			cv := want.Variants[j]
			wantFit := "red"
			if float64(cv.VRAMEstMB) <= float64(capMB)*0.85 {
				wantFit = "green"
			} else if cv.VRAMEstMB <= capMB {
				wantFit = "yellow"
			}
			if v.Tag != cv.Tag || v.VRAMEstMB != cv.VRAMEstMB || v.SizeMB != cv.SizeMB || v.Fit != wantFit {
				t.Errorf("%s variant %d = %+v, want tag %s vram %d size %d fit %s", got.Name, j, v, cv.Tag, cv.VRAMEstMB, cv.SizeMB, wantFit)
			}
		}
	}
}

// TestHandleModelCatalog_HFEstimateGolden pins absolute numbers, derived by hand
// rather than through the production helpers: qwen2.5:7b is 14525 MiB of weights;
// the estimate is weights x 1.2 (17430) plus 8192 context tokens x 0.2 MiB
// (1638.4) = 19068.4, truncated to 19068 MiB. On an 80 GiB (81920 MiB) node that
// is about 23%, green. The 8192-token context constant is pinned here by the
// 19068 figure, so changing hfListContextTokens fails this test.
func TestHandleModelCatalog_HFEstimateGolden(t *testing.T) {
	n := hfNodeEntry(t, "vllm", 80*1024)
	v := modelFor(t, n, "qwen2.5:7b").Variants[0]
	if v.SizeMB != 14525 || v.VRAMEstMB != 19068 || v.Fit != "green" {
		t.Errorf("qwen2.5:7b on 80 GiB = size %d vram %d fit %q, want 14525 / 19068 / green", v.SizeMB, v.VRAMEstMB, v.Fit)
	}
}

func TestCatalogHFVariant_NonPositiveSizeIsUnknown(t *testing.T) {
	for _, size := range []int64{0, -1} {
		t.Run(fmt.Sprintf("size %d", size), func(t *testing.T) {
			withHFSize(t, "qwen2.5:7b", size)
			v, fit, ok := catalogHFVariant("qwen2.5:7b", "vllm", 80*1024*1024*1024, "declared")
			if !ok || fit != "unknown" || v.VRAMEstMB != 0 || v.Recommended {
				t.Errorf("variant %+v fit %q ok %v, want unknown fit, no estimate, not recommended", v, fit, ok)
			}
		})
	}
	if v, _, ok := catalogHFVariant("qwen2.5:7b", "tgi", 80*1024*1024*1024, "declared"); !ok || !v.Recommended || v.VRAMEstMB == 0 {
		t.Errorf("sized variant = %+v ok %v, want recommended with an estimate", v, ok)
	}
}

func TestHandleModelCatalog_HFSizeUnknownCases(t *testing.T) {
	declared80 := func(n *router.NodeState) {
		n.VRAMTotalMB = 80 * 1024
		n.VRAMSource = "declared"
	}
	withDisk := func(freeGB, totalGB float64) func(n *router.NodeState) {
		return func(n *router.NodeState) {
			declared80(n)
			n.AgentPresent = true
			n.DiskFreeGB = freeGB
			n.DiskTotalGB = totalGB
		}
	}
	tests := []struct {
		name     string
		runtime  string
		mutate   func(n *router.NodeState)
		wantDisk string
	}{
		{"vllm no disk telemetry", "vllm", declared80, "unknown"},
		{"tgi no disk telemetry", "tgi", declared80, "unknown"},
		{"vllm unknown size on a nearly full disk is not ok", "vllm", withDisk(2, 100), "insufficient"},
		{"tgi unknown size on a nearly full disk is not ok", "tgi", withDisk(2, 100), "insufficient"},
		{"vllm unknown size with ample disk is still unknown, never ok", "vllm", withDisk(500, 1000), "unknown"},
		{"unknown VRAM and unknown size", "vllm", func(n *router.NodeState) {}, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withHFSize(t, "qwen2.5:7b", 0)
			m := modelFor(t, hfNodeEntryWith(t, tc.runtime, tc.mutate), "qwen2.5:7b")
			v := m.Variants[0]
			if v.Fit != "unknown" || v.DiskFit != tc.wantDisk || v.VRAMEstMB != 0 {
				t.Errorf("variant = fit %q disk %q vram %d, want fit unknown disk %q vram 0", v.Fit, v.DiskFit, v.VRAMEstMB, tc.wantDisk)
			}
			if m.Recommendation.Picked || m.Recommendation.Reason != reasonVRAMUnknown {
				t.Errorf("recommendation = %+v, want vram_unknown and no pick", m.Recommendation)
			}
		})
	}
}

func TestHandleModelCatalog_HFRowDiskInsufficient(t *testing.T) {
	n := hfNodeEntryWith(t, "vllm", func(n *router.NodeState) {
		n.VRAMTotalMB = 80 * 1024
		n.VRAMSource = "declared"
		n.AgentPresent = true
		n.DiskFreeGB = 10 // the 7B repo is about 15 GB
		n.DiskTotalGB = 100
	})
	m := modelFor(t, n, "qwen2.5:7b")
	if v := m.Variants[0]; v.DiskFit != "insufficient" || v.Fit != "green" {
		t.Errorf("variant = fit %q disk %q, want green / insufficient", v.Fit, v.DiskFit)
	}
	if got := m.Recommendation; got.Picked || got.Reason != reasonDiskInsufficient {
		t.Errorf("recommendation = %+v, want disk_insufficient", got)
	}
}

func TestHandleModelCatalog_HFMultiGPUUsesLargestGPU(t *testing.T) {
	n := hfNodeEntryWith(t, "vllm", func(n *router.NodeState) {
		n.AgentPresent = true
		n.VRAMSource = "agent"
		n.VRAMTotalMB = 4 * 24576
		n.AgentGPUs = []marboragent.GPUInfo{
			{Index: 0, VRAMTotalMB: 24576}, {Index: 1, VRAMTotalMB: 24576},
			{Index: 2, VRAMTotalMB: 24576}, {Index: 3, VRAMTotalMB: 24576},
		}
	})
	if n.VRAMFitBasis != "largest" {
		t.Fatalf("vram_fit_basis = %q, want largest", n.VRAMFitBasis)
	}
	// 14B BF16 needs about 35 GB: it fits the 96 GB total but not one 24 GB card.
	if got := modelFor(t, n, "qwen2.5:14b").Recommendation; got.Picked || got.Reason != reasonTooLarge {
		t.Errorf("14B on 4x24 GB = %+v, want too_large (sized against one GPU)", got)
	}
	if got := modelFor(t, n, "qwen2.5:7b").Recommendation; !got.Picked {
		t.Errorf("7B on 4x24 GB = %+v, want picked", got)
	}
}

func TestHandleModelCatalog_UnrecognisedRuntimeGetsNoHFRows(t *testing.T) {
	for _, rt := range []string{"", "triton", "VLLM"} {
		n := hfNodeEntry(t, rt, 80*1024)
		if len(n.Models) == 0 {
			t.Fatalf("runtime %q: node lists no models", rt)
		}
		for _, m := range n.Models {
			repo := catalogHFRepos[m.Name].Repo
			for _, v := range m.Variants {
				if repo != "" && v.Tag == repo {
					t.Errorf("runtime %q: %s shows the Hugging Face row, only vllm and tgi may", rt, m.Name)
				}
			}
		}
	}
}

// bannedHTTPSelectors are the net/http package-level names that use the
// process-wide default client or transport.
var bannedHTTPSelectors = map[string]bool{
	"DefaultClient": true, "DefaultTransport": true,
	"Get": true, "Post": true, "Head": true, "PostForm": true,
}

// httpDefaultClientUses returns the position of every net/http default-client
// selector (http.DefaultClient, http.Get, ...) in one Go source file.
func httpDefaultClientUses(filename, src string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" && bannedHTTPSelectors[sel.Sel.Name] {
			hits = append(hits, fmt.Sprintf("%s: http.%s", fset.Position(sel.Pos()), sel.Sel.Name))
		}
		return true
	})
	return hits, nil
}

func TestOutboundHTTPSourceGuard_DetectsViolation(t *testing.T) {
	src := "package admin\nimport \"net/http\"\nfunc f() { _, _ = http.Get(\"x\"); _ = http.DefaultClient }\n"
	hits, err := httpDefaultClientUses("x.go", src)
	if err != nil || len(hits) != 2 {
		t.Fatalf("want 2 hits, got %v (err %v)", hits, err)
	}
}

// TestOutboundHTTPSourceGuard fails if non-test code in package admin reaches
// the network through the process-wide default client. The handler-level
// no-outbound-HTTP test only swaps the shared Hugging Face client (swapping the
// global default client raced with a leaked goroutine from another test), so
// this source guard catches a new code path that bypasses the shared client.
func TestOutboundHTTPSourceGuard(t *testing.T) {
	// File name -> reason it may use the default client. Currently empty.
	allow := map[string]string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := allow[name]; ok {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := httpDefaultClientUses(name, string(b))
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			t.Errorf("default HTTP client used outside the shared client: %s", h)
		}
	}
}
