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
		log.Printf("marboragent: host telemetry (CPU/RAM/disk) is not implemented on this platform; host fields will be omitted (R1: omitted, never fabricated)")
	})
}
