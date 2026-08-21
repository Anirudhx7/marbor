package marboragent

// AMD ROCm GPUCollector, parsing `rocm-smi -a --json` output. UNVERIFIED ON
// REAL HARDWARE (Anirudh's explicit call, 2026-07-20 - no AMD card available
// in this environment): field parsing here is built against ROCm's publicly
// documented `-a --json` key names, not captured from an actual card, and
// rocm-smi's exact key spelling has genuinely drifted across ROCm releases
// (e.g. "Card series" vs "Card Series"). rocmField below tries every known
// variant per logical value for that reason. Treat this collector as needing
// a real-hardware validation pass before being fully trusted - if it never
// reports a device on a host that clearly has one, that's the first thing to
// check, not an inherent flaw. See gpu_nvidia.go for the identical shape this
// deliberately mirrors, and gpu.go for the GPUCollector contract both follow.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type rocmCollector struct{}

func (rocmCollector) Name() string { return "rocm" }

func (rocmCollector) Available(ctx context.Context) bool {
	_, err := lookPath("rocm-smi")
	return err == nil
}

func (rocmCollector) Collect(ctx context.Context) (GPUBlock, error) {
	out, err := exec.CommandContext(ctx, "rocm-smi", "-a", "--json").Output()
	if err != nil {
		return GPUBlock{}, fmt.Errorf("rocm-smi: %w", err)
	}
	block, ok := parseROCmSMIJSON(out)
	if !ok {
		return GPUBlock{}, fmt.Errorf("rocm-smi: could not parse output")
	}
	return block, nil
}

var rocmCardKeyPattern = regexp.MustCompile(`^card(\d+)$`)

// rocmField tries every known spelling of a logical value in turn and
// returns the first one actually present with a non-empty value - a real
// but differently-capitalized/renamed key across ROCm versions is never
// mistaken for an absent one, and a genuinely absent value stays absent
// rather than being defaulted (R1).
func rocmField(card map[string]string, names ...string) (string, bool) {
	for _, n := range names {
		if v, ok := card[n]; ok && strings.TrimSpace(v) != "" {
			return v, true
		}
	}
	return "", false
}

// parseROCmSMIJSON parses `rocm-smi -a --json`, whose top level is an object
// keyed "card0", "card1", ... (not an array, unlike nvidia-smi's XML <gpu>
// list) - each value a flat string-to-string map of every requested metric.
// Sorted numerically by card index so GPUInfo.Index stays stable and
// consistent with parseNvidiaSMIXML's array-position convention.
func parseROCmSMIJSON(data []byte) (GPUBlock, bool) {
	var raw map[string]map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return GPUBlock{}, false
	}

	var cardIDs []string
	for k := range raw {
		if rocmCardKeyPattern.MatchString(k) {
			cardIDs = append(cardIDs, k)
		}
	}
	if len(cardIDs) == 0 {
		return GPUBlock{}, false
	}
	sort.Slice(cardIDs, func(i, j int) bool {
		ni, _ := strconv.Atoi(rocmCardKeyPattern.FindStringSubmatch(cardIDs[i])[1])
		nj, _ := strconv.Atoi(rocmCardKeyPattern.FindStringSubmatch(cardIDs[j])[1])
		return ni < nj
	})

	block := GPUBlock{Count: len(cardIDs), Vendor: "rocm", Devices: make([]GPUInfo, 0, len(cardIDs))}
	for i, id := range cardIDs {
		card := raw[id]
		info := GPUInfo{Index: i, Vendor: "rocm"}

		if model, ok := rocmField(card, "Card series", "Card Series", "Card model", "Device Name"); ok {
			info.Model = strings.TrimSpace(model)
		}
		if temp, ok := rocmField(card, "Temperature (Sensor edge) (C)", "Temperature (Sensor junction) (C)", "Temperature (edge) (C)"); ok {
			if v, err := strconv.ParseFloat(strings.TrimSpace(temp), 64); err == nil {
				info.TemperatureC = &v
			}
		}
		if fan, ok := rocmField(card, "Fan speed (%)", "Fan Speed (%)"); ok {
			if v, err := strconv.ParseFloat(strings.TrimSpace(fan), 64); err == nil {
				info.FanPercent = &v
			}
		}
		if power, ok := rocmField(card, "Average Graphics Package Power (W)", "Current Socket Graphics Package Power (W)"); ok {
			if v, err := strconv.ParseFloat(strings.TrimSpace(power), 64); err == nil {
				info.PowerWatts = &v
			}
		}
		if util, ok := rocmField(card, "GPU use (%)", "GPU Use (%)"); ok {
			if v, err := strconv.ParseFloat(strings.TrimSpace(util), 64); err == nil {
				info.CorePercent = &v
			}
		}
		if total, ok := rocmField(card, "VRAM Total Memory (B)", "GPU Memory Total (Bytes)"); ok {
			if v, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64); err == nil {
				info.VRAMTotalMB = v / (1024 * 1024)
			}
		}
		if used, ok := rocmField(card, "VRAM Total Used Memory (B)", "GPU Memory Used (Bytes)"); ok {
			if v, err := strconv.ParseInt(strings.TrimSpace(used), 10, 64); err == nil {
				info.VRAMUsedMB = v / (1024 * 1024)
			}
		}
		block.Devices = append(block.Devices, info)
	}
	return block, true
}
