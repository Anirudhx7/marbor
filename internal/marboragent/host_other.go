//go:build !linux

package marboragent

// collectHost is a no-op stub on platforms without a zero-dependency stdlib
// path to CPU/RAM/disk (Windows, macOS, ...). Per the build spec: omit
// fields rather than add a dependency (gopsutil, cgo) or fabricate a number
// (R1). Every field stays nil/zero, which HostTelemetry's omitempty tags
// turn into an absent JSON field - the marbor-side consumer already treats
// that as "unknown" and falls back to "-".
func collectHost() *HostTelemetry {
	logHostUnsupported()
	return &HostTelemetry{}
}
