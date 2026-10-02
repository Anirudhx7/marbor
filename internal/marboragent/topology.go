package marboragent

import (
	"net"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Topology is raw, provenance-tagged evidence that one runtime process was
// launched as part of a multi-host deployment. The agent reports only what the
// launch itself says; it never decides which hosts belong together. Every
// unknown value is absent (nil pointer or empty), never a zero.
//
// A Topology is normally emitted only when the launch states distributed
// intent. The one exception is a llama.cpp process whose RPC server list could
// not be read from its environment: that is sent as Evidence alone
// ("env-unreadable:LLAMA_ARG_RPC") so "could not tell" is never confused with
// "no RPC servers".
type Topology struct {
	// Launcher names the mechanism the evidence came from:
	// "vllm-mp" (vLLM multiprocessing flags) or "llamacpp-rpc" (llama.cpp
	// --rpc server list).
	Launcher string `json:"launcher,omitempty"`
	// RoleHint is set only when the command line literally says so:
	// "worker" for vLLM's --headless.
	RoleHint string `json:"role_hint,omitempty"`
	// NNodes, NodeRank and MasterPort are the integers the command line gave.
	NNodes     *int `json:"nnodes,omitempty"`
	NodeRank   *int `json:"node_rank,omitempty"`
	MasterPort *int `json:"master_port,omitempty"`
	// MasterAddr is the --master-addr value exactly as typed (host or IP only).
	MasterAddr string `json:"master_addr,omitempty"`
	// RPCServers is the llama.cpp RPC server list, host:port entries in the
	// order given. Unusable entries are dropped and the count is capped.
	RPCServers []string `json:"rpc_servers,omitempty"`
	// Evidence lists where each value came from, for example
	// "cmdline:--nnodes", "docker-cmd:--headless", "env:LLAMA_ARG_RPC",
	// "docker-env:LLAMA_ARG_RPC" or "env-unreadable:LLAMA_ARG_RPC".
	Evidence []string `json:"evidence,omitempty"`
}

const (
	// maxRPCServers caps how many llama.cpp RPC entries are kept.
	maxRPCServers = 64
	// maxTopologyValueLen caps any single value taken from a command line or
	// environment.
	maxTopologyValueLen = 255
	// maxNodeCount bounds node counts and ranks to something a real fleet
	// could have, so a typo or hostile value is dropped rather than relayed.
	maxNodeCount = 4096
	maxPort      = 65535
)

// topologyEnvKeys is the complete list of environment variables the agent
// reads for topology evidence, separate from the GPU scope list. Everything
// else in a runtime's environment is discarded in memory.
var topologyEnvKeys = []string{"LLAMA_ARG_RPC"}

// topologyEnvList extracts only the topology variables from KEY=VALUE entries.
func topologyEnvList(entries []string) map[string]string {
	out := make(map[string]string)
	for _, e := range entries {
		idx := strings.Index(e, "=")
		if idx <= 0 {
			continue
		}
		key := e[:idx]
		for _, allowed := range topologyEnvKeys {
			if key == allowed {
				out[key] = e[idx+1:]
				break
			}
		}
	}
	return out
}

// topologyInput is everything buildTopology may look at. Only the named flags
// and allowlisted variables are ever read from it.
type topologyInput struct {
	// runtime is the runtime recognised from the process's own arguments, not
	// the name of a detected runtime it may later be matched to.
	runtime string
	tokens  []string
	// argvSource prefixes evidence for flags: "cmdline" or "docker-cmd".
	argvSource string
	// env holds the allowlisted topology variables, or nil when none were
	// read. envSource is "env" or "docker-env".
	env       map[string]string
	envSource string
	// envUnreadable is true when reading the runtime's environment was asked
	// for and failed.
	envUnreadable bool
}

// buildTopology returns the topology evidence for one runtime process, or nil
// when the launch does not state distributed intent.
func buildTopology(in topologyInput) *Topology {
	switch in.runtime {
	case "vllm":
		return vllmTopology(in)
	case "llamacpp":
		return llamaCppTopology(in)
	}
	return nil
}

func vllmTopology(in topologyInput) *Topology {
	t := &Topology{Launcher: "vllm-mp"}
	note := func(flag string) { t.Evidence = append(t.Evidence, in.argvSource+":"+flag) }

	if n, ok := intFlag(in.tokens, "--nnodes", 1, maxNodeCount); ok {
		t.NNodes = &n
		note("--nnodes")
	}
	if n, ok := intFlag(in.tokens, "--node-rank", 0, maxNodeCount-1); ok {
		t.NodeRank = &n
		note("--node-rank")
	}
	if v, ok := flagValue(in.tokens, "--master-addr"); ok && validHost(v) {
		t.MasterAddr = v
		note("--master-addr")
	}
	if n, ok := intFlag(in.tokens, "--master-port", 1, maxPort); ok {
		t.MasterPort = &n
		note("--master-port")
	}
	if hasFlag(in.tokens, "--headless") {
		t.RoleHint = "worker"
		note("--headless")
	}

	distributed := (t.NNodes != nil && *t.NNodes > 1) || t.NodeRank != nil || t.MasterAddr != "" || t.RoleHint != ""
	if !distributed {
		return nil
	}
	return t
}

func llamaCppTopology(in topologyInput) *Topology {
	t := &Topology{Launcher: "llamacpp-rpc"}
	// The command line overrides the environment in llama.cpp, so an --rpc
	// argument, even an unusable one, means the environment is not consulted.
	if v, ok := flagValue(in.tokens, "--rpc"); ok {
		if servers := parseRPCList(v); len(servers) > 0 {
			t.RPCServers = servers
			t.Evidence = append(t.Evidence, in.argvSource+":--rpc")
		}
	} else if in.envUnreadable {
		t.Evidence = append(t.Evidence, "env-unreadable:LLAMA_ARG_RPC")
	} else if v, ok := in.env["LLAMA_ARG_RPC"]; ok {
		if servers := parseRPCList(v); len(servers) > 0 {
			t.RPCServers = servers
			t.Evidence = append(t.Evidence, in.envSource+":LLAMA_ARG_RPC")
		}
	}
	if len(t.RPCServers) == 0 && len(t.Evidence) == 0 {
		return nil
	}
	return t
}

// flagValue finds the value of a command-line flag in either "--flag value" or
// "--flag=value" form. Only whole tokens are compared, so flag-like text inside
// another value (a model path) never matches. When a flag repeats, the last
// occurrence wins, as it does in the runtimes' own argument parsers. A flag
// with no value, or whose next token is another flag, is treated as absent.
func flagValue(tokens []string, name string) (string, bool) {
	prefix := name + "="
	var val string
	found := false
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch {
		case tok == name:
			if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
				val, found = tokens[i+1], true
				i++
			}
		case strings.HasPrefix(tok, prefix):
			val, found = tok[len(prefix):], true
		}
	}
	return val, found
}

// hasFlag reports whether a boolean flag is present as a whole token.
func hasFlag(tokens []string, name string) bool {
	for _, tok := range tokens {
		if tok == name || tok == name+"=true" {
			return true
		}
	}
	return false
}

// intFlag reads a flag as a plain decimal integer within [lo, hi].
func intFlag(tokens []string, name string, lo, hi int) (int, bool) {
	v, ok := flagValue(tokens, name)
	if !ok || !isDigits(v) || len(v) > 9 {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// validHost accepts a DNS-style name or an IP literal and nothing else: no
// credentials, scheme, path, whitespace or control characters.
func validHost(s string) bool {
	if s == "" || len(s) > maxTopologyValueLen || !utf8.ValidString(s) {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// parseHostPort accepts exactly host:port (or [v6]:port) and returns it as
// typed.
func parseHostPort(entry string) (string, bool) {
	if entry == "" || len(entry) > maxTopologyValueLen || !utf8.ValidString(entry) {
		return "", false
	}
	if strings.ContainsAny(entry, "@/\\ \t\r\n") {
		return "", false
	}
	host, port, err := net.SplitHostPort(entry)
	if err != nil || !validHost(host) || !isDigits(port) || len(port) > 5 {
		return "", false
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > maxPort {
		return "", false
	}
	return entry, true
}

// parseRPCList splits a comma separated llama.cpp RPC server list, keeping
// usable host:port entries in order up to the cap.
func parseRPCList(v string) []string {
	var out []string
	for _, raw := range strings.Split(v, ",") {
		if len(out) >= maxRPCServers {
			break
		}
		if entry, ok := parseHostPort(strings.TrimSpace(raw)); ok {
			out = append(out, entry)
		}
	}
	return out
}

// splitCmdline splits the NUL separated contents of /proc/<pid>/cmdline.
func splitCmdline(raw []byte) []string {
	var out []string
	for _, tok := range strings.Split(string(raw), "\x00") {
		if tok != "" {
			out = append(out, tok)
		}
	}
	return out
}
