package router

// agent_sanitize.go - bounds applied to one decoded agent status report before
// any of it is stored on a node. The agent is a separate process on another
// machine; a buggy or hostile one must not be able to make the router hold
// unbounded lists or strings, or place a node using an absurd VRAM size or a
// percentage outside 0-100.

import (
	"math"
	"unicode/utf8"

	"github.com/Anirudhx7/marbor/internal/marboragent"
)

const (
	// maxTelemetryMB caps any MB-denominated size (1<<40 MB is one exabyte,
	// far above any real device) so later multiplication into bytes cannot
	// overflow an int64.
	maxTelemetryMB int64 = 1 << 40
	// maxTelemetryListLen caps every list in a report.
	maxTelemetryListLen = 64
	// maxTelemetryStringBytes caps every string in a report.
	maxTelemetryStringBytes = 256
)

func clampMB(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > maxTelemetryMB {
		return maxTelemetryMB
	}
	return v
}

func clampStr(s string) string {
	if len(s) <= maxTelemetryStringBytes {
		return s
	}
	cut := maxTelemetryStringBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func clampStrs(in []string) []string {
	if in == nil {
		return nil
	}
	if len(in) > maxTelemetryListLen {
		in = in[:maxTelemetryListLen]
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = clampStr(s)
	}
	return out
}

// clampPercent bounds a percentage to 0-100. A non-finite value is unknown (nil).
func clampPercent(p *float64) *float64 {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	v := math.Min(100, math.Max(0, *p))
	return &v
}

// finiteNonNeg keeps a counter or reading only when it is finite and not negative.
func finiteNonNeg(p *float64) *float64 {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 {
		return nil
	}
	return p
}

func clampFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// sanitizeTelemetry bounds every size, percentage, list and string in t in
// place. Values outside bounds are clamped; non-finite readings become unknown.
func sanitizeTelemetry(t *marboragent.Telemetry) {
	t.Agent.NodeID = clampStr(t.Agent.NodeID)
	t.Agent.Version = clampStr(t.Agent.Version)
	t.Agent.Build = clampStr(t.Agent.Build)
	t.Agent.Platform = clampStr(t.Agent.Platform)
	t.Agent.Architecture = clampStr(t.Agent.Architecture)
	t.Capabilities = clampStrs(t.Capabilities)
	if h := t.Host; h != nil {
		h.CPUPercent = clampPercent(h.CPUPercent)
		h.RAMUsedMB = clampMB(h.RAMUsedMB)
		h.RAMTotalMB = clampMB(h.RAMTotalMB)
		h.DiskFreeGB = clampFloat(h.DiskFreeGB)
		h.DiskTotalGB = clampFloat(h.DiskTotalGB)
		h.Hostname = clampStr(h.Hostname)
		h.Addrs = clampStrs(h.Addrs)
	}
	if g := t.GPU; g != nil {
		g.Vendor = clampStr(g.Vendor)
		g.DriverVersion = clampStr(g.DriverVersion)
		g.CUDAVersion = clampStr(g.CUDAVersion)
		if g.Count < 0 {
			g.Count = 0
		}
		if g.Count > maxTelemetryListLen {
			g.Count = maxTelemetryListLen
		}
		if len(g.Devices) > maxTelemetryListLen {
			g.Devices = g.Devices[:maxTelemetryListLen]
		}
		for i := range g.Devices {
			d := &g.Devices[i]
			d.Vendor = clampStr(d.Vendor)
			d.Model = clampStr(d.Model)
			d.CorePercent = clampPercent(d.CorePercent)
			d.FanPercent = clampPercent(d.FanPercent)
			d.TemperatureC = finiteNonNeg(d.TemperatureC)
			d.PowerWatts = finiteNonNeg(d.PowerWatts)
			d.VRAMUsedMB = clampMB(d.VRAMUsedMB)
			d.VRAMTotalMB = clampMB(d.VRAMTotalMB)
		}
	}
	if len(t.Runtimes) > maxTelemetryListLen {
		t.Runtimes = t.Runtimes[:maxTelemetryListLen]
	}
	sanitizeRuntime(t.Runtime)
	for i := range t.Runtimes {
		sanitizeRuntime(&t.Runtimes[i])
	}
	if c := t.Control; c != nil {
		c.Driver = clampStr(c.Driver)
		c.Capabilities = clampStrs(c.Capabilities)
		if d := c.Discovered; d != nil {
			d.Driver = clampStr(d.Driver)
			d.Identifier = clampStr(d.Identifier)
			d.Evidence = clampStrs(d.Evidence)
		}
	}
	if len(t.Deployments) > maxTelemetryListLen {
		t.Deployments = t.Deployments[:maxTelemetryListLen]
	}
	for i := range t.Deployments {
		d := &t.Deployments[i]
		d.Runtime = clampStr(d.Runtime)
		d.RuntimeID = clampStr(d.RuntimeID)
		d.Source = clampStr(d.Source)
		if len(d.GPUGroup) > maxTelemetryListLen {
			d.GPUGroup = d.GPUGroup[:maxTelemetryListLen]
		}
		if s := d.GPUScope; s != nil {
			if len(s.Indices) > maxTelemetryListLen {
				s.Indices = s.Indices[:maxTelemetryListLen]
			}
			s.UUIDs = clampStrs(s.UUIDs)
			s.Raw = clampStr(s.Raw)
			s.Source = clampStr(s.Source)
			s.Note = clampStr(s.Note)
		}
		if p := d.Parallelism; p != nil {
			p.Type = clampStr(p.Type)
		}
	}
}

func sanitizeRuntime(rt *marboragent.RuntimeInfo) {
	if rt == nil {
		return
	}
	rt.Name = clampStr(rt.Name)
	rt.ID = clampStr(rt.ID)
	rt.Version = clampStr(rt.Version)
	rt.Status = clampStr(rt.Status)
	rt.WarmModels = clampStrs(rt.WarmModels)
	if e := rt.Engine; e != nil {
		e.KVCacheUsagePercent = clampPercent(e.KVCacheUsagePercent)
		e.PrefixCacheQueries = finiteNonNeg(e.PrefixCacheQueries)
		e.PrefixCacheHits = finiteNonNeg(e.PrefixCacheHits)
	}
}
