package marboragent

import (
	"net"
	"sort"
)

// maxHostAddrs caps how many addresses one host reports.
const maxHostAddrs = 32

// ifaceAddrs is one network interface's flags and addresses, split out from
// net.Interface so address filtering can be tested without a real network.
type ifaceAddrs struct {
	flags net.Flags
	addrs []net.Addr
}

// localHostAddrs returns this host's own usable addresses. It uses only the
// standard library, so it works on every OS the agent runs on. An agent that
// runs inside a bridged container reports that container's addresses, not the
// machine's.
func localHostAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	list := make([]ifaceAddrs, 0, len(ifaces))
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		list = append(list, ifaceAddrs{flags: ifaces[i].Flags, addrs: addrs})
	}
	return collectHostAddrs(list)
}

// collectHostAddrs keeps unicast addresses of interfaces that are up and not
// loopback, dropping link-local addresses, then sorts, de-duplicates and caps
// them. It returns nil when nothing qualifies, meaning unknown.
func collectHostAddrs(list []ifaceAddrs) []string {
	seen := make(map[string]bool)
	var out []string
	for _, ifc := range list {
		if ifc.flags&net.FlagUp == 0 || ifc.flags&net.FlagLoopback != 0 {
			continue
		}
		for _, a := range ifc.addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			// IsGlobalUnicast is false for loopback, link-local, multicast
			// and unspecified addresses; private ranges stay.
			if ip == nil || !ip.IsGlobalUnicast() {
				continue
			}
			s := ip.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	if len(out) > maxHostAddrs {
		out = out[:maxHostAddrs]
	}
	return out
}
