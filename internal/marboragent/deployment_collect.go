package marboragent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// deploymentCollector finds the inference runtimes running on this host and
// reports their parallelism flags and GPU scope. Every outside dependency is
// a field so tests run without real processes, /proc or nvidia-smi.
type deploymentCollector struct {
	// readRuntimeEnv is the explicit operator opt-in to read another
	// process's environment (/proc/<pid>/environ). Off by default: that file
	// can hold secrets, and reading it needs the same user or root.
	readRuntimeEnv bool
	ps             func() (string, error)
	readEnviron    func(pid int) ([]byte, error)
	run            commandRunner
	dockerSocket   string
}

func newDeploymentCollector(readRuntimeEnv bool) *deploymentCollector {
	return &deploymentCollector{
		readRuntimeEnv: readRuntimeEnv,
		ps:             psList,
		readEnviron:    readProcEnviron,
		run:            runCommand,
		dockerSocket:   "/var/run/docker.sock",
	}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

var errEnvironUnsupported = errors.New("reading a runtime's environment is only supported on Linux")

func readProcEnviron(pid int) ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, errEnvironUnsupported
	}
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
}

// psList lists every process as "pid ppid args" lines. Isolated for tests.
func psList() (string, error) {
	for _, cmd := range [][]string{{"ps", "-eo", "pid,ppid,args"}, {"ps", "-A", "-o", "pid,ppid,command"}} {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err == nil {
			return string(out), nil
		}
	}
	return "", fmt.Errorf("ps failed")
}

// Collect returns one report per runtime instance it can see, or nil when it
// cannot see any (isolated PID namespace, no docker socket). Nil means
// unknown, never "no deployment".
func (c *deploymentCollector) Collect(detected []DetectedRuntime) []DeploymentReport {
	psOut, err := c.ps()
	if err == nil && strings.TrimSpace(psOut) != "" {
		if reps := c.collectFromPS(psOut, detected); len(reps) > 0 {
			return reps
		}
	}
	// ps saw no runtime process: it may be running in another PID namespace
	// (a container), so ask docker. The ps tree still lets us follow a
	// container's init process to its children.
	return c.collectFromDockerSocket(detected, buildProcTree(psOut))
}

// procTree maps a parent PID to its direct children.
type procTree map[int][]int

func buildProcTree(psOutput string) procTree {
	tree := procTree{}
	for _, line := range strings.Split(psOutput, "\n") {
		p := parsePSLine(line)
		if p.pid > 0 && p.ppid > 0 {
			tree[p.ppid] = append(tree[p.ppid], p.pid)
		}
	}
	return tree
}

// withDescendants returns pid and every process below it.
func (t procTree) withDescendants(pid int) []int {
	out := []int{pid}
	for i := 0; i < len(out) && len(out) < 4096; i++ {
		out = append(out, t[out[i]]...)
	}
	return out
}

type psProc struct {
	pid, ppid int
	args      string
}

// parsePSLine reads "pid ppid args". A line that does not start with two
// integers (a header, or a ps variant with other columns) keeps the whole
// line as args with pid 0, meaning "pid unknown".
func parsePSLine(line string) psProc {
	line = strings.TrimSpace(line)
	f := strings.Fields(line)
	if len(f) >= 3 {
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 == nil && e2 == nil {
			return psProc{pid: pid, ppid: ppid, args: strings.Join(f[2:], " ")}
		}
	}
	return psProc{args: line}
}

// scopeProbe lazily runs the NVIDIA probe at most once per collection pass.
type scopeProbe struct {
	run   commandRunner
	done  bool
	probe *nvidiaProbe
}

func (s *scopeProbe) get() *nvidiaProbe {
	if !s.done {
		s.done = true
		if p, err := probeNVIDIA(s.run); err == nil {
			s.probe = p
		}
	}
	return s.probe
}

// collectFromPS builds reports from `ps -eo pid,ppid,args` output.
func (c *deploymentCollector) collectFromPS(psOutput string, detected []DetectedRuntime) []DeploymentReport {
	var reports []DeploymentReport
	portToRuntime := make(map[int]string)
	for _, d := range detected {
		if d.Port > 0 {
			portToRuntime[d.Port] = d.Name
		}
	}
	tree := buildProcTree(psOutput)
	probe := &scopeProbe{run: c.run}
	for _, line := range strings.Split(psOutput, "\n") {
		p := parsePSLine(line)
		if p.args == "" {
			continue
		}
		// Heuristic: only consider lines that look like inference runtimes
		lower := strings.ToLower(p.args)
		if !strings.Contains(lower, "vllm") && !strings.Contains(lower, "sglang") && !strings.Contains(lower, "tgi") && !strings.Contains(lower, "llama") && !strings.Contains(lower, "ollama") {
			continue
		}
		runtimeHint := detectRuntimeFromArgs(lower)
		if runtimeHint == "" {
			continue
		}
		par, caps := parseParallelismFromArgs(runtimeHint, p.args)
		port := extractPortFromArgs(p.args)
		// If we have a detected runtime on same port, prefer its Name
		if port > 0 {
			if name, ok := portToRuntime[port]; ok {
				runtimeHint = name
			}
		} else if len(detected) == 1 {
			// Single runtime host - attribute lone detection's port
			port = detected[0].Port
			if detected[0].Name != "" {
				runtimeHint = detected[0].Name
			}
		}
		rep := DeploymentReport{
			Runtime:     runtimeHint,
			Port:        port,
			Parallelism: par,
			Caps:        caps,
			Source:      "ps",
		}
		rep.GPUScope, rep.GPUGroup = c.scopeForProcess(p.pid, tree, probe)
		reports = appendDeduped(reports, rep)
	}
	return reports
}

// appendDeduped keeps one report per port+runtime, preferring the one that
// carries parallelism.
func appendDeduped(reports []DeploymentReport, rep DeploymentReport) []DeploymentReport {
	for i, existing := range reports {
		if existing.Port == rep.Port && existing.Runtime == rep.Runtime {
			if existing.Parallelism == nil && rep.Parallelism != nil {
				reports[i] = rep
			}
			return reports
		}
	}
	return append(reports, rep)
}

// scopeForProcess learns the GPU scope of the runtime process pid: its own
// environment when the operator opted in, plus the NVIDIA per-process
// cross-check when nvidia-smi is available. The agent's own environment is
// never consulted: it says nothing about another process.
func (c *deploymentCollector) scopeForProcess(pid int, tree procTree, probe *scopeProbe) (*GPUScope, []int) {
	var (
		sc     *GPUScope
		legacy []int
	)
	if c.readRuntimeEnv && pid > 0 {
		sc, legacy = c.scopeFromProcessEnv(pid)
	}
	if pid > 0 {
		sc, legacy = applyCrossCheck(sc, legacy, probe.get(), tree.withDescendants(pid))
	}
	return sc, legacy
}

func (c *deploymentCollector) scopeFromProcessEnv(pid int) (*GPUScope, []int) {
	blob, err := c.readEnviron(pid)
	if err != nil {
		note := "the runtime's environment is not readable (the agent needs the same user or root)"
		if errors.Is(err, errEnvironUnsupported) {
			note = "the runtime's environment is not readable on this OS (supported on Linux only)"
		}
		return &GPUScope{Note: note}, nil
	}
	sc, legacy := scopeFromEnv(allowlistedEnv(blob), "environ:")
	if sc == nil {
		return &GPUScope{Source: "environ", Note: "no GPU visibility variable is set in the runtime's environment, so it can see every GPU"}, nil
	}
	return sc, legacy
}

// dockerContainer holds the minimal fields we need from docker inspect.
type dockerContainer struct {
	State struct {
		Pid int `json:"Pid"`
	} `json:"State"`
	Config struct {
		Env   []string `json:"Env"`
		Cmd   []string `json:"Cmd"`
		Image string   `json:"Image"`
	} `json:"Config"`
	Args            []string `json:"Args"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

// dockerDeployment is a parsed container plus the host PID of its init
// process, which the cross-check uses.
type dockerDeployment struct {
	report DeploymentReport
	pid    int
}

// parseDockerInspect parses a single docker inspect JSON blob. It keeps only
// the allowlisted GPU variables from the container's configured environment
// and never invents a GPU group.
func parseDockerInspect(inspectJSON string) *dockerDeployment {
	var cont dockerContainer
	if err := json.Unmarshal([]byte(inspectJSON), &cont); err != nil {
		return nil
	}
	argsStr := strings.Join(append(append([]string(nil), cont.Config.Cmd...), cont.Args...), " ")
	if argsStr == "" {
		argsStr = cont.Config.Image
	}
	runtimeHint := detectRuntimeFromArgs(strings.ToLower(argsStr + " " + cont.Config.Image))
	if runtimeHint == "" {
		if img := strings.ToLower(cont.Config.Image); strings.Contains(img, "vllm") {
			runtimeHint = "vllm"
		} else if strings.Contains(img, "tgi") {
			runtimeHint = "tgi"
		}
	}
	if runtimeHint == "" {
		return nil
	}
	par, caps := parseParallelismFromArgs(runtimeHint, argsStr)
	port := extractPortFromArgs(argsStr)
	if port == 0 {
		for _, bindings := range cont.NetworkSettings.Ports {
			for _, b := range bindings {
				if p, err := strconv.Atoi(b.HostPort); err == nil && p > 0 {
					port = p
					break
				}
			}
		}
	}
	rep := DeploymentReport{
		Runtime:     runtimeHint,
		Port:        port,
		Parallelism: par,
		Caps:        caps,
		Source:      "docker",
	}
	rep.GPUScope, rep.GPUGroup = scopeFromEnv(allowlistedEnvList(cont.Config.Env), "docker-env:")
	return &dockerDeployment{report: rep, pid: cont.State.Pid}
}

// collectFromDockerSocket queries the docker socket if present. Returns nil if
// the socket is absent or no deployments were found (unknown, not fabricated).
func (c *deploymentCollector) collectFromDockerSocket(detected []DetectedRuntime, tree procTree) []DeploymentReport {
	sock := c.dockerSocket
	if _, err := os.Stat(sock); err != nil {
		return nil
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 3 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json", nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var list []struct {
		Id string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil
	}
	probe := &scopeProbe{run: c.run}
	var reports []DeploymentReport
	for _, ct := range list {
		if !dockerContainerIDRE.MatchString(ct.Id) {
			continue
		}
		body, ok := inspectContainer(client, ct.Id)
		if !ok {
			continue
		}
		d := parseDockerInspect(body)
		if d == nil {
			continue
		}
		if d.pid > 0 {
			d.report.GPUScope, d.report.GPUGroup = applyCrossCheck(d.report.GPUScope, d.report.GPUGroup, probe.get(), tree.withDescendants(d.pid))
		}
		reports = append(reports, d.report)
	}
	return filterToDetected(reports, detected)
}

func inspectContainer(client *http.Client, id string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+id+"/json", nil)
	if err != nil {
		return "", false
	}
	rs, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer rs.Body.Close()
	if rs.StatusCode != http.StatusOK {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(rs.Body, 256*1024))
	if err != nil {
		return "", false
	}
	return string(body), true
}

// filterToDetected keeps reports that match a detected runtime port where
// possible, falling back to everything found.
func filterToDetected(reports []DeploymentReport, detected []DetectedRuntime) []DeploymentReport {
	if len(reports) == 0 {
		return nil
	}
	portSet := make(map[int]bool)
	for _, d := range detected {
		if d.Port > 0 {
			portSet[d.Port] = true
		}
	}
	if len(portSet) == 0 {
		return reports
	}
	var filtered []DeploymentReport
	for _, r := range reports {
		if r.Port == 0 || portSet[r.Port] {
			filtered = append(filtered, r)
		}
	}
	if len(filtered) > 0 {
		return filtered
	}
	return reports
}
