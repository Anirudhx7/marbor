package marboragent

// Apple Metal (Apple Silicon / macOS) GPUCollector, via `system_profiler
// SPDisplaysDataType -json`. UNVERIFIED ON REAL HARDWARE (no Mac available
// in this environment, 2026-07-20): built against
// system_profiler's publicly documented JSON shape, same caveat as
// gpu_rocm.go/gpu_intel.go.
//
// Deliberately reports Model only, every numeric reading left unset rather
// than guessed: system_profiler runs unprivileged and exposes no
// temperature/fan/power/utilization for the GPU at all, and unlike a
// discrete card, Apple Silicon's GPU shares the host's unified memory rather
// than owning a separate VRAM pool - there is no real "VRAM used/total"
// figure to report without conflating it with host RAM (which
// HostTelemetry already reports separately, and would double-count if
// echoed here as VRAM too). The privileged alternative for temperature/
// power (`powermetrics`) needs root, which this agent must not assume.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type appleCollector struct{}

func (appleCollector) Name() string { return "apple" }

func (appleCollector) Available(ctx context.Context) bool {
	_, err := lookPath("system_profiler")
	return err == nil
}

// spDisplaysResponse mirrors `system_profiler SPDisplaysDataType -json`'s
// documented shape: a "SPDisplaysDataType" array, one entry per GPU, each
// entry's own "_name" field giving the chipset/model (e.g. "Apple M3 Max").
type spDisplaysResponse struct {
	SPDisplaysDataType []spDisplaysEntry `json:"SPDisplaysDataType"`
}

type spDisplaysEntry struct {
	Name string `json:"_name"`
}

func (appleCollector) Collect(ctx context.Context) (GPUBlock, error) {
	out, err := exec.CommandContext(ctx, "system_profiler", "SPDisplaysDataType", "-json").Output()
	if err != nil {
		return GPUBlock{}, fmt.Errorf("system_profiler: %w", err)
	}
	block, ok := parseSPDisplaysJSON(out)
	if !ok {
		return GPUBlock{}, fmt.Errorf("system_profiler: could not parse output")
	}
	return block, nil
}

func parseSPDisplaysJSON(data []byte) (GPUBlock, bool) {
	var resp spDisplaysResponse
	if err := json.Unmarshal(data, &resp); err != nil || len(resp.SPDisplaysDataType) == 0 {
		return GPUBlock{}, false
	}

	block := GPUBlock{Count: len(resp.SPDisplaysDataType), Vendor: "apple", Devices: make([]GPUInfo, 0, len(resp.SPDisplaysDataType))}
	for i, entry := range resp.SPDisplaysDataType {
		block.Devices = append(block.Devices, GPUInfo{
			Index:  i,
			Vendor: "apple",
			Model:  strings.TrimSpace(entry.Name),
		})
	}
	return block, true
}
