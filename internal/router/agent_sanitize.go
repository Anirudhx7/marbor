package router

// agent_sanitize.go - bounds applied to one decoded agent status report before
// any of it is stored on a node. The agent is a separate process on another
// machine; a buggy or hostile one must not be able to make the router hold
// unbounded lists or strings, or place a node using an absurd VRAM size, a
// negative counter or a percentage outside 0-100.

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Anirudhx7/marbor/internal/marboragent"
)

const (
	// maxTelemetryMB caps any MB-denominated size. 1<<30 MB is a petabyte, far
	// above any real device, and small enough that the sum over the capped
	// device, multiplied into bytes, fits an int64. A sum over the up to 64
	// devices is not bounded by this cap alone, so the router clamps the
	// aggregate it stores as well.
	maxTelemetryMB int64 = 1 << 30
	// maxTelemetryListLen caps every list in a report.
	maxTelemetryListLen = 64
	// maxTelemetryStringBytes caps every string in a report.
	maxTelemetryStringBytes = 256
	// maxTelemetryCount caps every plain counter (queue depth, running
	// requests, node counts, parallelism widths).
	maxTelemetryCount = 1 << 20
	// maxTelemetryGPUIndex caps a GPU index value.
	maxTelemetryGPUIndex = 1023
	// maxTelemetryDiskGB caps a disk size in GB (a petabyte-scale figure).
	maxTelemetryDiskGB = float64(1 << 30)
	maxPort            = 65535
)

// isUnsafeRune reports whether c must not appear in a stored or logged string:
// control characters (including newlines and escapes), the Unicode line and
// paragraph separators, and the bidirectional overrides and isolates that can
// make text display as something else.
func isUnsafeRune(c rune) bool {
	if unicode.IsControl(c) {
		return true
	}
	switch {
	case c == 0x2028, c == 0x2029, c == 0x200E, c == 0x200F, c == 0x061C:
		return true
	case c >= 0x202A && c <= 0x202E:
		return true
	case c >= 0x2066 && c <= 0x2069:
		return true
	}
	return false
}

func clampMB(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > maxTelemetryMB {
		return maxTelemetryMB
	}
	return v
}

func clampCount(v int) int {
	if v < 0 {
		return 0
	}
	if v > maxTelemetryCount {
		return maxTelemetryCount
	}
	return v
}

func clampPort(v int) int {
	if v < 0 || v > maxPort {
		return 0
	}
	return v
}

// clampCountPtr keeps a counter only when it is in range; otherwise it is unknown.
func clampCountPtr(p *int) *int {
	if p == nil || *p < 0 || *p > maxTelemetryCount {
		return nil
	}
	return p
}

func clampPortPtr(p *int) *int {
	if p == nil || *p < 0 || *p > maxPort {
		return nil
	}
	return p
}

// clampStr drops unsafe characters and invalid UTF-8, treats a whitespace-only
// value as empty, and cuts the rest to maxTelemetryStringBytes on a character
// boundary. Leading and trailing whitespace of a non-empty value is kept.
func clampStr(s string) string {
	s = strings.Map(func(c rune) rune {
		if isUnsafeRune(c) {
			return -1
		}
		return c
	}, strings.ToValidUTF8(s, ""))
	if strings.TrimSpace(s) == "" {
		return ""
	}
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

// clampIndices returns a copy of in when every value is a valid GPU index and
// the list is within the cap. Otherwise it returns nil (unknown): keeping only
// the good entries would leave a shorter group next to a width that still says
// more.
func clampIndices(in []int) []int {
	if in == nil || len(in) > maxTelemetryListLen {
		return nil
	}
	for _, v := range in {
		if v < 0 || v > maxTelemetryGPUIndex {
			return nil
		}
	}
	return append([]int(nil), in...)
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

func clampDiskGB(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return math.Min(v, maxTelemetryDiskGB)
}

// maxTelemetrySeconds caps uptime and boot time (epoch seconds): about 540
// years, far past any real value.
const maxTelemetrySeconds int64 = 1 << 34

func clampSeconds(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > maxTelemetrySeconds {
		return maxTelemetrySeconds
	}
	return v
}

// sanitizeTelemetry bounds every size, percentage, counter, list and string in
// t in place. Values outside bounds are clamped (or dropped when clamping would
// invent a figure); non-finite readings become unknown. It runs once, right
// after a status reply is decoded and before any of it is stored. That is the
// only place a decoded report enters the router, so every storing path
// (including the one that follows a TLS check) sees bounded data.
//
// GPU.Count is the count the agent reports. It is kept when it exceeds the
// device list (a collection that failed this cycle) but raised to the number of
// devices when it is smaller. The aggregate fields the router derives from
// device 0 read the first entry, which truncation to the list cap keeps.
func sanitizeTelemetry(t *marboragent.Telemetry) {
	t.Agent.NodeID = clampStr(t.Agent.NodeID)
	t.Agent.Version = clampStr(t.Agent.Version)
	t.Agent.Build = clampStr(t.Agent.Build)
	t.Agent.Platform = clampStr(t.Agent.Platform)
	t.Agent.Architecture = clampStr(t.Agent.Architecture)
	t.Agent.ProtocolVersion = clampCount(t.Agent.ProtocolVersion)
	t.Capabilities = clampStrs(t.Capabilities)
	if h := t.Host; h != nil {
		h.CPUPercent = clampPercent(h.CPUPercent)
		h.RAMUsedMB = clampMB(h.RAMUsedMB)
		h.RAMTotalMB = clampMB(h.RAMTotalMB)
		if h.RAMTotalMB > 0 && h.RAMUsedMB > h.RAMTotalMB {
			h.RAMUsedMB = h.RAMTotalMB
		}
		h.DiskFreeGB = clampDiskGB(h.DiskFreeGB)
		h.DiskTotalGB = clampDiskGB(h.DiskTotalGB)
		h.Hostname = clampStr(h.Hostname)
		h.UptimeSeconds = clampSeconds(h.UptimeSeconds)
		h.BootTime = clampSeconds(h.BootTime)
		h.Addrs = clampStrs(h.Addrs)
	}
	if g := t.GPU; g != nil {
		sanitizeGPU(g)
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
		sanitizeDeployment(&t.Deployments[i])
	}
}

func sanitizeGPU(g *marboragent.GPUBlock) {
	g.Vendor = clampStr(g.Vendor)
	g.DriverVersion = clampStr(g.DriverVersion)
	g.CUDAVersion = clampStr(g.CUDAVersion)
	if len(g.Devices) > maxTelemetryListLen {
		g.Devices = g.Devices[:maxTelemetryListLen]
	}
	for i := range g.Devices {
		d := &g.Devices[i]
		d.Index = clampCount(d.Index)
		if d.Index > maxTelemetryGPUIndex {
			d.Index = maxTelemetryGPUIndex
		}
		d.Vendor = clampStr(d.Vendor)
		d.Model = clampStr(d.Model)
		d.CorePercent = clampPercent(d.CorePercent)
		d.FanPercent = clampPercent(d.FanPercent)
		d.TemperatureC = finiteNonNeg(d.TemperatureC)
		d.PowerWatts = finiteNonNeg(d.PowerWatts)
		d.VRAMUsedMB = clampMB(d.VRAMUsedMB)
		d.VRAMTotalMB = clampMB(d.VRAMTotalMB)
		// Used never exceeds a known total. With no total reported the used reading
		// is left as read: some collectors report the two independently.
		if d.VRAMTotalMB > 0 && d.VRAMUsedMB > d.VRAMTotalMB {
			d.VRAMUsedMB = d.VRAMTotalMB
		}
	}
	g.Count = clampCount(g.Count)
	if g.Count > maxTelemetryListLen {
		g.Count = maxTelemetryListLen
	}
	if g.Count < len(g.Devices) {
		g.Count = len(g.Devices)
	}
}

func sanitizeDeployment(d *marboragent.DeploymentReport) {
	d.Runtime = clampStr(d.Runtime)
	d.RuntimeID = clampStr(d.RuntimeID)
	d.Source = clampStr(d.Source)
	d.Port = clampPort(d.Port)
	d.GPUGroup = clampIndices(d.GPUGroup)
	if s := d.GPUScope; s != nil {
		s.Indices = clampIndices(s.Indices)
		s.UUIDs = clampStrs(s.UUIDs)
		s.Raw = clampStr(s.Raw)
		s.Source = clampStr(s.Source)
		s.Note = clampStr(s.Note)
	}
	if p := d.Parallelism; p != nil {
		p.Type = clampStr(p.Type)
		p.Width = clampCount(p.Width)
		p.PipelineWidth = clampCount(p.PipelineWidth)
		p.DataWidth = clampCount(p.DataWidth)
	}
	if c := d.Caps; c != nil {
		c.MaxTPWidth = clampCount(c.MaxTPWidth)
	}
	if tp := d.Topology; tp != nil {
		tp.Launcher = clampStr(tp.Launcher)
		tp.RoleHint = clampStr(tp.RoleHint)
		tp.MasterAddr = clampStr(tp.MasterAddr)
		tp.RPCServers = clampStrs(tp.RPCServers)
		tp.Evidence = clampStrs(tp.Evidence)
		tp.NNodes = clampCountPtr(tp.NNodes)
		tp.NodeRank = clampCountPtr(tp.NodeRank)
		tp.MasterPort = clampPortPtr(tp.MasterPort)
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
	rt.Port = clampPort(rt.Port)
	rt.QueueDepth = clampCount(rt.QueueDepth)
	rt.WarmModels = clampStrs(rt.WarmModels)
	if e := rt.Engine; e != nil {
		e.RunningRequests = clampCountPtr(e.RunningRequests)
		e.WaitingRequests = clampCountPtr(e.WaitingRequests)
		e.KVCacheUsagePercent = clampPercent(e.KVCacheUsagePercent)
		e.PrefixCacheQueries = finiteNonNeg(e.PrefixCacheQueries)
		e.PrefixCacheHits = finiteNonNeg(e.PrefixCacheHits)
	}
}
