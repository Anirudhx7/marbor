package main

// engine.go - engine-side evidence and the prefix-only dispatcher.
//
// The router's own claims (which node it picked, what it scored) are one
// source of evidence. The engines' counters are a second, independent one:
// how many prompt tokens each engine really served from its prefix cache. A
// run is only meaningful if both agree that the intended behavior happened.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// engineSnapshot is one reading of one engine's cumulative counters.
type engineSnapshot struct {
	Name         string  `json:"name"`
	OK           bool    `json:"ok"`
	Err          string  `json:"err,omitempty"`
	PromptTokens float64 `json:"prompt_tokens"`
	CachedTokens float64 `json:"cached_tokens"`
	Requests     float64 `json:"requests"`
	Raw          []byte  `json:"-"`
}

// engineDelta is the change in an engine's counters across one run.
type engineDelta struct {
	Name         string  `json:"name"`
	Valid        bool    `json:"valid"`
	PromptTokens float64 `json:"prompt_tokens"`
	CachedTokens float64 `json:"cached_tokens"`
	CachedFrac   float64 `json:"cached_frac"`
}

func fetchBytes(hc *http.Client, url string) ([]byte, error) {
	resp, err := hc.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return b, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return b, nil
}

// probeEngine reads one engine according to kind ("sim", "vllm" or "none").
func probeEngine(hc *http.Client, kind string, en engineSpec) engineSnapshot {
	snap := engineSnapshot{Name: en.Name}
	switch kind {
	case "sim":
		raw, err := fetchBytes(hc, en.URL+"/sim/stats")
		if err != nil {
			snap.Err = err.Error()
			return snap
		}
		var s struct {
			Requests     float64 `json:"requests"`
			PromptTokens float64 `json:"prompt_tokens"`
			CachedTokens float64 `json:"cached_tokens"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			snap.Err = err.Error()
			return snap
		}
		snap.OK, snap.Raw = true, raw
		snap.Requests, snap.PromptTokens, snap.CachedTokens = s.Requests, s.PromptTokens, s.CachedTokens
	case "vllm":
		raw, err := fetchBytes(hc, en.URL+"/metrics")
		if err != nil {
			snap.Err = err.Error()
			return snap
		}
		samples := parseProm(raw)
		q, okq := sumMetric(samples, "vllm:prefix_cache_queries")
		h, okh := sumMetric(samples, "vllm:prefix_cache_hits")
		snap.Raw = raw
		if !okq || !okh {
			snap.Err = "vllm prefix cache counters not exposed (metrics lack vllm:prefix_cache_queries/hits)"
			return snap
		}
		snap.OK, snap.PromptTokens, snap.CachedTokens = true, q, h
		if r, ok := sumMetric(samples, "vllm:request_success"); ok {
			snap.Requests = r
		}
	default:
		snap.Err = "engine probing disabled"
	}
	return snap
}

func probeAll(hc *http.Client, kind string, engines []engineSpec) []engineSnapshot {
	out := make([]engineSnapshot, len(engines))
	for i, en := range engines {
		out[i] = probeEngine(hc, kind, en)
	}
	return out
}

func diffSnapshots(before, after []engineSnapshot) []engineDelta {
	out := make([]engineDelta, len(after))
	for i := range after {
		d := engineDelta{Name: after[i].Name}
		if i < len(before) && before[i].OK && after[i].OK {
			d.Valid = true
			d.PromptTokens = after[i].PromptTokens - before[i].PromptTokens
			d.CachedTokens = after[i].CachedTokens - before[i].CachedTokens
			if d.PromptTokens > 0 {
				d.CachedFrac = d.CachedTokens / d.PromptTokens
			}
		}
		out[i] = d
	}
	return out
}

// promSample is one Prometheus text-format sample with its labels dropped.
type promSample struct {
	name  string
	value float64
}

// parseProm reads Prometheus text exposition. Labels are ignored; callers sum
// across label sets (one engine instance serves one model, so this is safe).
func parseProm(raw []byte) []promSample {
	var out []promSample
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var name, rest string
		if i := strings.IndexByte(line, '{'); i >= 0 {
			j := strings.LastIndexByte(line, '}')
			if j < i {
				continue
			}
			name, rest = line[:i], line[j+1:]
		} else if f := strings.Fields(line); len(f) >= 2 {
			name, rest = f[0], strings.Join(f[1:], " ")
		} else {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		out = append(out, promSample{name, v})
	}
	return out
}

// sumMetric sums a counter whether or not the exposition adds a _total suffix.
func sumMetric(samples []promSample, base string) (float64, bool) {
	var sum float64
	found := false
	for _, s := range samples {
		if s.name == base || s.name == base+"_total" {
			sum += s.value
			found = true
		}
	}
	return sum, found
}

// pickEngine is the prefix-only arm's dispatcher: rendezvous hashing of the
// conversation head over the engine names. Every request with the same head
// goes to the same engine, regardless of how busy it is.
func pickEngine(key string, engines []engineSpec) engineSpec {
	var best engineSpec
	var bestScore uint64
	for i, en := range engines {
		sum := sha256.Sum256([]byte(key + "\x00" + en.Name))
		if s := binary.BigEndian.Uint64(sum[:8]); i == 0 || s > bestScore {
			best, bestScore = en, s
		}
	}
	return best
}
