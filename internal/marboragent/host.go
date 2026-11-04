package marboragent

import (
	"log"
	"sync"
)

// hostUnsupportedOnce ensures the "host telemetry not implemented on this
// platform" notice is logged at most once per process, per the build spec's
// "log at startup once, don't guess" instruction for platforms where a
// zero-dependency approach to a field isn't available.
var hostUnsupportedOnce sync.Once

func logHostUnsupported() {
	hostUnsupportedOnce.Do(func() {
		log.Printf("marboragent: host telemetry (CPU/RAM/disk) is not implemented on this platform; host fields will be omitted (never fabricated)")
	})
}

// maxUptimeSeconds bounds a plausible /proc/uptime reading (about 317 years);
// anything larger is treated as unknown rather than converted to int64.
const maxUptimeSeconds = 1e10

// parseUptimeSeconds extracts whole seconds of uptime from the first field of
// /proc/uptime, rejecting non-finite, negative and absurdly large values.
func parseUptimeSeconds(field string) (int64, bool) {
	v, ok := parseFiniteFloat(field)
	if !ok || v < 0 || v > maxUptimeSeconds {
		return 0, false
	}
	return int64(v), true
}
