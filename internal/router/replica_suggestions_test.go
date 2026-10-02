package router

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/store"
)

func intp(v int) *int { return &v }

func testRow(name, host string, port int, runtime string) nodeView {
	return nodeView{Name: name, Host: host, Port: port, Runtime: runtime}
}

// vllmDep builds a vLLM deployment report carrying multiprocessing evidence.
func vllmDep(port int, rank, nnodes int, master string, masterPort int, headless bool) marboragent.DeploymentReport {
	t := &marboragent.Topology{
		Launcher:   LauncherVLLMMultiprocessing,
		NNodes:     intp(nnodes),
		NodeRank:   intp(rank),
		MasterAddr: master,
		Evidence:   []string{"cmdline:--nnodes"},
	}
	if masterPort > 0 {
		t.MasterPort = intp(masterPort)
	}
	if headless {
		t.RoleHint = "worker"
	}
	return marboragent.DeploymentReport{Runtime: "vllm", Port: port, Topology: t}
}

func rpcDep(port int, servers ...string) marboragent.DeploymentReport {
	return marboragent.DeploymentReport{
		Runtime: "llamacpp", Port: port,
		Topology: &marboragent.Topology{Launcher: LauncherLlamaCPPRPC, RPCServers: servers, Evidence: []string{"cmdline:--rpc"}},
	}
}

func hostEv(host, hostname string, addrs []string, deps ...marboragent.DeploymentReport) HostEvidence {
	return HostEvidence{
		Host: host, Hostname: hostname, Addrs: addrs,
		Capabilities: []string{"status", "deployment.report", capTopology},
		Deployments:  deps,
	}
}

func evMap(evs ...HostEvidence) map[string]HostEvidence {
	m := make(map[string]HostEvidence, len(evs))
	for _, e := range evs {
		m[e.Host] = e
	}
	return m
}

// twoHostVLLM is the baseline: head on host-a (rank 0), headless worker on
// host-b (rank 1), master named by IP.
func twoHostVLLM() ([]nodeView, map[string]HostEvidence) {
	nodes := []nodeView{
		testRow("head", "host-a", 8000, "vllm"),
		testRow("worker", "host-b", 8000, "vllm"),
	}
	hosts := evMap(
		hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(8000, 0, 2, "10.0.0.1", 29500, false)),
		hostEv("host-b", "b", []string{"10.0.0.2"}, vllmDep(0, 1, 2, "10.0.0.1", 29500, true)),
	)
	return nodes, hosts
}

func findSuggestion(t *testing.T, got []ReplicaSuggestion, state string) ReplicaSuggestion {
	t.Helper()
	for _, s := range got {
		if s.State == state {
			return s
		}
	}
	t.Fatalf("no %s suggestion in %+v", state, got)
	return ReplicaSuggestion{}
}

func TestCorrelate_VLLM(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(nodes *[]nodeView, hosts map[string]HostEvidence)
		wantState string // "" = no suggestion at all
		wantHead  string
		wantMembs []string
		wantText  string // substring of Reason/Missing
	}{
		{name: "two nodes complete", wantState: SuggestionComplete, wantHead: "head", wantMembs: []string{"head", "worker"}},
		{
			name: "head only is incomplete and says rank 1 is unseen",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				delete(hosts, "host-b")
			},
			wantState: SuggestionIncomplete, wantHead: "head", wantMembs: []string{"head"}, wantText: "rank 1 has not been seen",
		},
		{
			name: "worker host not registered as a node",
			mutate: func(nodes *[]nodeView, hosts map[string]HostEvidence) {
				*nodes = (*nodes)[:1]
				delete(hosts, "host-b")
			},
			wantState: SuggestionIncomplete, wantHead: "head", wantMembs: []string{"head"}, wantText: "Register its host as a node",
		},
		{
			name: "rank collision is conflicting",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				hosts["host-b"] = hostEv("host-b", "b", []string{"10.0.0.2"}, vllmDep(0, 0, 2, "10.0.0.1", 29500, true))
			},
			wantState: SuggestionConflicting, wantText: "rank 0 is claimed by",
		},
		{
			name: "master host reports its node as headless",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				hosts["host-a"] = hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(8000, 0, 2, "10.0.0.1", 29500, true))
			},
			wantState: SuggestionConflicting, wantText: "does not report rank 0 as a head",
		},
		{
			name: "members disagree on node count",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				hosts["host-b"] = hostEv("host-b", "b", []string{"10.0.0.2"}, vllmDep(0, 1, 3, "10.0.0.1", 29500, true))
			},
			wantState: SuggestionConflicting, wantText: "disagree on the node count",
		},
		{
			name: "loopback master with several nodes is conflicting",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				hosts["host-a"] = hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(8000, 0, 2, "127.0.0.1", 29500, false))
				delete(hosts, "host-b")
			},
			wantState: SuggestionConflicting, wantText: "loopback",
		},
		{
			name: "one node launch is not a group",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				hosts["host-a"] = hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(8000, 0, 1, "10.0.0.1", 29500, false))
				delete(hosts, "host-b")
			},
		},
		{
			name: "master address on two hosts is ambiguous",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				hosts["host-a"] = hostEv("host-a", "a", []string{"172.17.0.1"}, vllmDep(8000, 0, 2, "172.17.0.1", 29500, false))
				hosts["host-b"] = hostEv("host-b", "b", []string{"172.17.0.1"}, vllmDep(0, 1, 2, "172.17.0.1", 29500, true))
			},
			wantState: SuggestionConflicting, wantText: "matches more than one host",
		},
		{
			name: "master named by fully qualified name matches agent hostname",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				hosts["host-a"] = hostEv("host-a", "gpu-a.lab.example", []string{"10.0.0.1"}, vllmDep(8000, 0, 2, "GPU-A.lab.example", 29500, false))
				hosts["host-b"] = hostEv("host-b", "b", []string{"10.0.0.2"}, vllmDep(0, 1, 2, "gpu-a.lab.example", 29500, true))
			},
			wantState: SuggestionComplete, wantHead: "head", wantMembs: []string{"head", "worker"},
		},
		{
			name: "master named exactly like the node host string",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				hosts["host-a"] = hostEv("host-a", "x", nil, vllmDep(8000, 0, 2, "HOST-A", 29500, false))
				hosts["host-b"] = hostEv("host-b", "y", nil, vllmDep(0, 1, 2, "host-a", 29500, true))
			},
			wantState: SuggestionComplete, wantHead: "head", wantMembs: []string{"head", "worker"},
		},
		{
			name: "master address matching nothing stays unmatched",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				hosts["host-a"] = hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(8000, 0, 2, "10.9.9.9", 29500, false))
				hosts["host-b"] = hostEv("host-b", "b", []string{"10.0.0.2"}, vllmDep(0, 1, 2, "10.9.9.9", 29500, true))
			},
			wantState: SuggestionIncomplete, wantText: "does not match any registered node or agent address",
		},
		{
			name: "worker without a rank is never inferred",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				d := vllmDep(0, 1, 2, "10.0.0.1", 29500, true)
				d.Topology.NodeRank = nil
				hosts["host-b"] = hostEv("host-b", "b", []string{"10.0.0.2"}, d)
			},
			wantState: SuggestionIncomplete, wantText: "does not state its rank",
		},
		{
			name: "member agent without the topology capability is not counted as reporting",
			mutate: func(_ *[]nodeView, hosts map[string]HostEvidence) {
				ev := hosts["host-b"]
				ev.Capabilities = []string{"status", "deployment.report"}
				hosts["host-b"] = ev
			},
			wantState: SuggestionIncomplete, wantText: "is not reporting launch details",
		},
		{
			name: "node registered with a different runtime is conflicting",
			mutate: func(nodes *[]nodeView, _ map[string]HostEvidence) {
				(*nodes)[0].Runtime = "llamacpp"
			},
			wantState: SuggestionConflicting, wantText: "registered as llamacpp",
		},
		{
			name: "auto runtime does not conflict",
			mutate: func(nodes *[]nodeView, _ map[string]HostEvidence) {
				(*nodes)[0].Runtime = "auto"
			},
			wantState: SuggestionComplete, wantHead: "head", wantMembs: []string{"head", "worker"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes, hosts := twoHostVLLM()
			if tt.mutate != nil {
				tt.mutate(&nodes, hosts)
			}
			got, _ := correlate(nodes, hosts)
			if tt.wantState == "" {
				if len(got) != 0 {
					t.Fatalf("want no suggestion, got %+v", got)
				}
				return
			}
			s := findSuggestion(t, got, tt.wantState)
			if tt.wantState == SuggestionComplete {
				if !s.Confirmable {
					t.Errorf("complete suggestion not confirmable")
				}
			} else if s.Confirmable {
				t.Errorf("%s suggestion must not be confirmable", s.State)
			}
			if tt.wantHead != "" && s.Head != tt.wantHead {
				t.Errorf("head = %q, want %q", s.Head, tt.wantHead)
			}
			if tt.wantMembs != nil && !reflect.DeepEqual(s.Members, tt.wantMembs) {
				t.Errorf("members = %v, want %v", s.Members, tt.wantMembs)
			}
			if tt.wantText != "" && !strings.Contains(s.Reason+" "+strings.Join(s.Missing, " "), tt.wantText) {
				t.Errorf("reason/missing %q %v lack %q", s.Reason, s.Missing, tt.wantText)
			}
		})
	}
}

func TestCorrelate_TwoMastersFormTwoGroups(t *testing.T) {
	nodes := []nodeView{
		testRow("a-head", "host-a", 8000, "vllm"),
		testRow("b-head", "host-b", 8000, "vllm"),
	}
	hosts := evMap(
		hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(8000, 0, 2, "10.0.0.1", 29500, false)),
		hostEv("host-b", "b", []string{"10.0.0.2"}, vllmDep(8000, 0, 2, "10.0.0.2", 29500, false)),
	)
	got, _ := correlate(nodes, hosts)
	if len(got) != 2 {
		t.Fatalf("want two separate groups, got %+v", got)
	}
	for _, s := range got {
		if s.Confirmable || len(s.Members) != 1 {
			t.Errorf("each lone master must be an unconfirmable single-member group, got %+v", s)
		}
	}
}

func TestCorrelate_HeadlessWorkerOnHostWithTwoRows(t *testing.T) {
	nodes, hosts := twoHostVLLM()
	nodes = append(nodes, testRow("worker-ollama", "host-b", 11434, "ollama"))
	got, _ := correlate(nodes, hosts)
	s := findSuggestion(t, got, SuggestionConflicting)
	if !reflect.DeepEqual(s.Members, []string{"worker", "worker-ollama"}) || !strings.Contains(s.Reason, "cannot be matched") {
		t.Errorf("unplaceable worker not reported: %+v", s)
	}
	for _, g := range got {
		if g.Confirmable {
			t.Errorf("nothing may be confirmable here: %+v", g)
		}
	}
}

func TestCorrelate_PortAttribution(t *testing.T) {
	// Two rows on one host: a report with a port goes to the row with that
	// port; a report with no port cannot be placed.
	nodes := []nodeView{
		testRow("ollama", "host-a", 11434, "ollama"),
		testRow("head", "host-a", 8000, "vllm"),
		testRow("worker", "host-b", 8000, "vllm"),
	}
	hosts := evMap(
		hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(8000, 0, 2, "10.0.0.1", 29500, false)),
		hostEv("host-b", "b", []string{"10.0.0.2"}, vllmDep(0, 1, 2, "10.0.0.1", 29500, true)),
	)
	got, _ := correlate(nodes, hosts)
	s := findSuggestion(t, got, SuggestionComplete)
	if !reflect.DeepEqual(s.Members, []string{"head", "worker"}) {
		t.Errorf("members = %v", s.Members)
	}
	// A report on a port no row owns is not attributed to any row.
	hosts["host-a"] = hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(9999, 0, 2, "10.0.0.1", 29500, false))
	got, _ = correlate(nodes, hosts)
	for _, s := range got {
		for _, m := range s.Members {
			if m == "head" || m == "ollama" {
				t.Errorf("report on an unowned port attributed to %s: %+v", m, s)
			}
		}
	}
}

func TestCorrelate_RuntimeIDAttribution(t *testing.T) {
	nodes, hosts := twoHostVLLM()
	nodes[0].Port = 0
	nodes[0].PinnedID = "rt-1"
	d := vllmDep(0, 0, 2, "10.0.0.1", 29500, false)
	d.RuntimeID = "rt-1"
	hosts["host-a"] = hostEv("host-a", "a", []string{"10.0.0.1"}, d)
	got, _ := correlate(nodes, hosts)
	findSuggestion(t, got, SuggestionComplete)
}

func TestCorrelate_AddressOnlyIdentifiesOneHost(t *testing.T) {
	// 10.0.0.1 appears in the addresses of both hosts: it identifies neither.
	nodes, hosts := twoHostVLLM()
	b := hosts["host-b"]
	b.Addrs = []string{"10.0.0.2", "10.0.0.1"}
	hosts["host-b"] = b
	got, _ := correlate(nodes, hosts)
	for _, s := range got {
		if s.Confirmable {
			t.Fatalf("an address on two hosts must not produce a confirmable group: %+v", s)
		}
	}
	findSuggestion(t, got, SuggestionConflicting)
}

func TestCorrelate_EvidenceForUnregisteredHostIsIgnored(t *testing.T) {
	nodes, hosts := twoHostVLLM()
	// host-c has no node row; its addresses must not feed identity.
	hosts["host-c"] = hostEv("host-c", "c", []string{"10.0.0.1"}, vllmDep(0, 1, 2, "10.0.0.1", 29500, true))
	got, _ := correlate(nodes, hosts)
	s := findSuggestion(t, got, SuggestionComplete)
	if len(s.Members) != 2 {
		t.Errorf("members = %v", s.Members)
	}
}

func TestCorrelate_LlamaCPP(t *testing.T) {
	baseNodes := func() []nodeView {
		return []nodeView{
			testRow("head", "host-a", 8080, "llamacpp"),
			testRow("w1", "host-b", 8080, "llamacpp"),
			testRow("w2", "host-c", 8080, "llamacpp"),
		}
	}
	hostsFor := func(servers ...string) map[string]HostEvidence {
		return evMap(
			hostEv("host-a", "a", []string{"10.0.0.1"}, rpcDep(8080, servers...)),
			hostEv("host-b", "b", []string{"10.0.0.2"}),
			hostEv("host-c", "c", []string{"10.0.0.3"}),
		)
	}
	tests := []struct {
		name      string
		servers   []string
		nodes     func() []nodeView
		wantState string
		wantMembs []string
		wantText  string
	}{
		{name: "complete", servers: []string{"10.0.0.2:50052", "10.0.0.3:50052"}, wantState: SuggestionComplete, wantMembs: []string{"head", "w1", "w2"}},
		{name: "one server not registered", servers: []string{"10.0.0.2:50052", "10.0.0.9:50052"}, wantState: SuggestionIncomplete, wantMembs: []string{"head", "w1"}, wantText: "1 RPC servers not registered as nodes"},
		{name: "head lists itself", servers: []string{"10.0.0.1:50052", "10.0.0.2:50052"}, wantState: SuggestionIncomplete, wantText: "lists itself"},
		{name: "same node listed twice", servers: []string{"10.0.0.2:50052", "10.0.0.2:50053"}, wantState: SuggestionIncomplete, wantText: "same node"},
		{name: "loopback mixed with network address", servers: []string{"127.0.0.1:50052", "10.0.0.2:50052"}, wantState: SuggestionConflicting, wantText: "loopback"},
		{name: "all loopback is single host", servers: []string{"127.0.0.1:50052", "localhost:50053"}},
		{
			name: "worker host with two rows cannot be told", servers: []string{"10.0.0.2:50052"},
			nodes: func() []nodeView {
				return append(baseNodes(), testRow("w1-other", "host-b", 11434, "ollama"))
			},
			wantState: SuggestionConflicting, wantText: "2 node rows",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := baseNodes()
			if tt.nodes != nil {
				nodes = tt.nodes()
			}
			got, _ := correlate(nodes, hostsFor(tt.servers...))
			if tt.wantState == "" {
				if len(got) != 0 {
					t.Fatalf("want none, got %+v", got)
				}
				return
			}
			s := findSuggestion(t, got, tt.wantState)
			if s.Head != "head" {
				t.Errorf("head = %q", s.Head)
			}
			if tt.wantMembs != nil && !reflect.DeepEqual(s.Members, tt.wantMembs) {
				t.Errorf("members = %v, want %v", s.Members, tt.wantMembs)
			}
			if tt.wantText != "" && !strings.Contains(s.Reason+strings.Join(s.Missing, " "), tt.wantText) {
				t.Errorf("reason %q missing %v lack %q", s.Reason, s.Missing, tt.wantText)
			}
			if s.Confirmable != (tt.wantState == SuggestionComplete) {
				t.Errorf("confirmable = %v for state %s", s.Confirmable, s.State)
			}
		})
	}
}

func TestCorrelate_LlamaCPPWorkerNeedsNoAgent(t *testing.T) {
	nodes := []nodeView{testRow("head", "host-a", 8080, "llamacpp"), testRow("w1", "10.0.0.2", 8080, "llamacpp")}
	hosts := evMap(hostEv("host-a", "a", []string{"10.0.0.1"}, rpcDep(8080, "10.0.0.2:50052")))
	got, cov := correlate(nodes, hosts)
	findSuggestion(t, got, SuggestionComplete)
	for _, c := range cov {
		if c.Node == "w1" && c.State != CoverageNoAgent {
			t.Errorf("worker coverage = %+v", c)
		}
	}
}

func TestCorrelate_DeclaredWins(t *testing.T) {
	declare := func(nodes []nodeView, name string, members []string, head string) {
		for i := range nodes {
			if nodes[i].Name == name {
				nodes[i].Declared = &store.ReplicaPeers{Members: members, Head: head}
			}
		}
	}
	t.Run("identical declaration on every member hides the suggestion", func(t *testing.T) {
		nodes, hosts := twoHostVLLM()
		declare(nodes, "head", []string{"worker", "head"}, "head")
		declare(nodes, "worker", []string{"head", "worker"}, "head")
		if got, _ := correlate(nodes, hosts); len(got) != 0 {
			t.Fatalf("already declared, got %+v", got)
		}
	})
	t.Run("a different declared head contradicts and is not confirmable", func(t *testing.T) {
		nodes, hosts := twoHostVLLM()
		declare(nodes, "head", []string{"head", "worker"}, "worker")
		got, _ := correlate(nodes, hosts)
		s := findSuggestion(t, got, SuggestionContradictsDeclared)
		if s.Confirmable || len(s.Declared) != 1 || s.Declared[0].Node != "head" || s.Declared[0].Head != "worker" {
			t.Errorf("contradiction not shown with its declared side: %+v", s)
		}
	})
	t.Run("a different declared member set contradicts", func(t *testing.T) {
		nodes, hosts := twoHostVLLM()
		declare(nodes, "worker", []string{"worker", "other"}, "worker")
		findSuggestion(t, mustCorrelate(nodes, hosts), SuggestionContradictsDeclared)
	})
	t.Run("identical on some members and none on others is still complete", func(t *testing.T) {
		nodes, hosts := twoHostVLLM()
		declare(nodes, "head", []string{"head", "worker"}, "head")
		s := findSuggestion(t, mustCorrelate(nodes, hosts), SuggestionComplete)
		if !s.Confirmable || len(s.Declared) != 1 {
			t.Errorf("got %+v", s)
		}
	})
	t.Run("an explicitly cleared declaration counts as none", func(t *testing.T) {
		nodes, hosts := twoHostVLLM()
		declare(nodes, "head", nil, "")
		findSuggestion(t, mustCorrelate(nodes, hosts), SuggestionComplete)
	})
}

func mustCorrelate(nodes []nodeView, hosts map[string]HostEvidence) []ReplicaSuggestion {
	s, _ := correlate(nodes, hosts)
	return s
}

func TestCorrelate_Coverage(t *testing.T) {
	nodes := []nodeView{
		testRow("reporting", "h1", 8000, "vllm"),
		testRow("silent", "h2", 8000, "vllm"),
		testRow("old", "h3", 8000, "vllm"),
		testRow("noagent", "h4", 8000, "vllm"),
		testRow("envless", "h5", 8080, "llamacpp"),
	}
	hosts := evMap(
		hostEv("h1", "h1", []string{"10.1.0.1"}, vllmDep(8000, 0, 2, "10.1.0.1", 29500, false)),
		hostEv("h2", "h2", []string{"10.1.0.2"}),
		HostEvidence{Host: "h3", Capabilities: []string{"status"}},
		hostEv("h5", "h5", nil, marboragent.DeploymentReport{
			Runtime: "llamacpp", Port: 8080,
			Topology: &marboragent.Topology{Evidence: []string{"env-unreadable:LLAMA_ARG_RPC"}},
		}),
	)
	got, cov := correlate(nodes, hosts)
	byNode := map[string]TopologyCoverage{}
	for _, c := range cov {
		byNode[c.Node] = c
	}
	if c := byNode["reporting"]; c.State != CoverageReporting || !c.Detected {
		t.Errorf("reporting: %+v", c)
	}
	if c := byNode["silent"]; c.State != CoverageReporting || c.Detected || c.Detail != "nothing detected" {
		t.Errorf("capability present with nothing found: %+v", c)
	}
	if c := byNode["old"]; c.State != CoverageAgentUpdateNeeded {
		t.Errorf("capability absent: %+v", c)
	}
	if c := byNode["noagent"]; c.State != CoverageNoAgent {
		t.Errorf("no evidence: %+v", c)
	}
	if c := byNode["envless"]; c.State != CoverageEnvUnreadable || c.Detected {
		t.Errorf("env unreadable: %+v", c)
	}
	// The unreadable environment must never form a group by itself.
	for _, s := range got {
		for _, m := range s.Members {
			if m == "envless" {
				t.Errorf("env-unreadable evidence produced a suggestion: %+v", s)
			}
		}
	}
}

func TestCorrelate_NilAndEmptyInputsNeverPanic(t *testing.T) {
	nodes := []nodeView{testRow("a", "h", 8000, "vllm"), {Name: "b", Host: "h2"}}
	hosts := evMap(
		HostEvidence{Host: "h"},
		hostEv("h2", "", nil,
			marboragent.DeploymentReport{Runtime: "vllm"},
			marboragent.DeploymentReport{Runtime: "vllm", Topology: &marboragent.Topology{}},
			marboragent.DeploymentReport{Runtime: "vllm", Topology: &marboragent.Topology{Launcher: LauncherVLLMMultiprocessing, MasterAddr: "x"}},
			marboragent.DeploymentReport{Runtime: "llamacpp", Topology: &marboragent.Topology{RPCServers: []string{""}}},
			marboragent.DeploymentReport{Topology: &marboragent.Topology{RoleHint: "worker"}},
		),
	)
	correlate(nil, nil)
	correlate(nodes, nil)
	correlate(nodes, hosts)
	correlate([]nodeView{{Name: "x", Host: ""}}, evMap(HostEvidence{Host: ""}))
}

func TestCorrelate_OrderIndependent(t *testing.T) {
	nodes := []nodeView{
		testRow("head", "host-a", 8000, "vllm"),
		testRow("worker", "host-b", 8000, "vllm"),
		testRow("lhead", "host-c", 8080, "llamacpp"),
		testRow("lw1", "host-d", 8080, "llamacpp"),
		testRow("lw2", "host-e", 8080, "llamacpp"),
		testRow("dup1", "host-f", 8000, "vllm"),
		testRow("dup2", "host-f", 8001, "vllm"),
	}
	hosts := evMap(
		hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(8000, 0, 2, "10.0.0.1", 29500, false)),
		hostEv("host-b", "b", []string{"10.0.0.2"}, vllmDep(0, 1, 2, "10.0.0.1", 29500, true)),
		hostEv("host-c", "c", []string{"10.0.0.3"}, rpcDep(8080, "10.0.0.4:50052", "10.0.0.5:50052")),
		hostEv("host-d", "d", []string{"10.0.0.4"}),
		hostEv("host-e", "e", []string{"10.0.0.5"}),
		hostEv("host-f", "f", []string{"10.0.0.6"}, vllmDep(0, 1, 2, "10.0.0.1", 29500, true), vllmDep(0, 1, 2, "10.0.0.1", 29500, true)),
	)
	wantS, wantC := correlate(nodes, hosts)
	if len(wantS) < 3 {
		t.Fatalf("fixture too small: %+v", wantS)
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		shuffled := append([]nodeView(nil), nodes...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		hs := make(map[string]HostEvidence, len(hosts))
		for k, v := range hosts {
			deps := append([]marboragent.DeploymentReport(nil), v.Deployments...)
			rng.Shuffle(len(deps), func(a, b int) { deps[a], deps[b] = deps[b], deps[a] })
			v.Deployments = deps
			hs[k] = v
		}
		gotS, gotC := correlate(shuffled, hs)
		if !reflect.DeepEqual(gotS, wantS) || !reflect.DeepEqual(gotC, wantC) {
			t.Fatalf("iteration %d: result depends on input order\n got %+v\nwant %+v", i, gotS, wantS)
		}
	}
}

func TestFingerprint(t *testing.T) {
	a := fingerprint("head", []string{"a", "head"})
	if len(a) != 16 {
		t.Fatalf("len = %d", len(a))
	}
	if a != fingerprint("head", []string{"a", "head"}) {
		t.Error("not stable")
	}
	if a == fingerprint("head", []string{"a", "b", "head"}) {
		t.Error("membership change must change the fingerprint")
	}
	if a == fingerprint("a", []string{"a", "head"}) {
		t.Error("head change must change the fingerprint")
	}
	// Nothing but head and members enters the hash, so a state change on the
	// same group keeps its fingerprint (and so its dismissal).
	nodes, hosts := twoHostVLLM()
	complete, _ := correlate(nodes, hosts)
	delete(hosts, "host-b")
	partial, _ := correlate(nodes, hosts)
	if len(complete) != 1 || len(partial) != 1 || complete[0].Fingerprint == partial[0].Fingerprint {
		t.Errorf("group with one fewer member must differ: %+v %+v", complete, partial)
	}
	nodes, hosts = twoHostVLLM()
	nodes[0].Declared = &store.ReplicaPeers{Members: []string{"head", "worker"}, Head: "worker"}
	contra, _ := correlate(nodes, hosts)
	if len(contra) != 1 || contra[0].State != SuggestionContradictsDeclared || contra[0].Fingerprint != complete[0].Fingerprint {
		t.Errorf("state change on the same members must keep the fingerprint: %+v vs %+v", contra, complete)
	}
}
