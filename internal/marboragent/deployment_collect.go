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
	"sort"
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
	// readCmdline returns /proc/<pid>/cmdline (NUL separated arguments), which
	// keeps argument boundaries that ps output loses.
	readCmdline  func(pid int) ([]byte, error)
	run          commandRunner
	dockerSocket string
}

func newDeploymentCollector(readRuntimeEnv bool) *deploymentCollector {
	return &deploymentCollector{
		readRuntimeEnv: readRuntimeEnv,
		ps:             psList,
		readEnviron:    readProcEnviron,
		readCmdline:    readProcCmdline,
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

// maxCmdlineBytes bounds how much of a process's command line is read.
const maxCmdlineBytes = 256 * 1024

func readProcCmdline(pid int) ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, errEnvironUnsupported
	}
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxCmdlineBytes))
}

// psTimeout bounds the whole process listing, so a hung ps cannot stall the
// scheduler refresh that waits on it.
var psTimeout = 4 * time.Second

// maxPSBytes bounds how much process listing is read. A host with a very large
// process table is truncated, which only loses processes at the end of the
// list, never corrupts the lines that were read.
const maxPSBytes = 4 * 1024 * 1024

// psRunner runs one ps invocation and returns its standard output only.
type psRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// limitedBuffer keeps at most max bytes and silently drops the rest. Write
// always reports success so the child is never blocked on a full pipe.
type limitedBuffer struct {
	buf []byte
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

// runPS runs a command with stdout captured up to maxPSBytes. Standard error
// is discarded so a warning can never be parsed as a process line.
func runPS(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out := &limitedBuffer{max: maxPSBytes}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.buf, nil
}

// psList lists every process as "pid ppid args" lines. Isolated for tests.
func psList() (string, error) { return psListWith(runPS) }

func psListWith(run psRunner) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	for _, cmd := range [][]string{{"ps", "-eo", "pid,ppid,args"}, {"ps", "-A", "-o", "pid,ppid,command"}} {
		out, err := run(ctx, cmd[0], cmd[1:]...)
		if err == nil {
			return string(out), nil
		}
		if ctx.Err() != nil {
			break
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
		// Only a process whose executable (or python module) is a runtime
		// counts; a client command that merely names one does not.
		lower := strings.ToLower(p.args)
		runtimeHint := processRuntime(lower)
		if runtimeHint == "" {
			continue
		}
		par, caps := parseParallelismFromArgs(runtimeHint, p.args)
		port := extractPortFromArgs(p.args)
		// Topology uses the runtime the process's own arguments name, before
		// any renaming to a detected runtime below.
		topo := c.topologyForProcess(runtimeHint, p)
		// A headless worker serves no HTTP, so it never borrows a port.
		headless := topo != nil && topo.RoleHint == "worker"
		// If we have a detected runtime on same port, prefer its Name
		if port > 0 {
			if name, ok := portToRuntime[port]; ok {
				runtimeHint = name
			}
		} else if len(detected) == 1 && !headless && detected[0].Name == runtimeHint {
			// The one detected runtime is this same runtime: its port is this
			// process's port. A different runtime's port or name is never lent.
			port = detected[0].Port
		}
		rep := DeploymentReport{
			Runtime:     runtimeHint,
			Port:        port,
			Parallelism: par,
			Caps:        caps,
			Source:      "ps",
			Topology:    topo,
		}
		rep.GPUScope, rep.GPUGroup = c.scopeForProcess(p.pid, tree, probe)
		reports = appendDeduped(reports, rep)
	}
	return reports
}

// appendDeduped keeps one report per port+runtime, preferring the one that
// carries parallelism or topology evidence. Reports with no known port share
// the key port 0, so several same-runtime processes without a port (for
// example the worker ranks of one launch) collapse into one report: without a
// port nothing can tell the instances apart, and the server fans reports out by
// port.
func appendDeduped(reports []DeploymentReport, rep DeploymentReport) []DeploymentReport {
	for i, existing := range reports {
		if existing.Port == rep.Port && existing.Runtime == rep.Runtime {
			if (existing.Parallelism == nil && rep.Parallelism != nil) || (existing.Topology == nil && rep.Topology != nil) {
				reports[i] = rep
			}
			return reports
		}
	}
	return append(reports, rep)
}

// argvTokens returns the process's arguments as separate tokens. It prefers
// /proc/<pid>/cmdline, which keeps argument boundaries, but only when that
// agrees with the ps line (so a pid reused since ps ran cannot lend its
// arguments to this report); otherwise it splits the ps line.
func (c *deploymentCollector) argvTokens(p psProc) []string {
	if p.pid > 0 && c.readCmdline != nil {
		if raw, err := c.readCmdline(p.pid); err == nil {
			toks := splitCmdline(raw)
			joined := strings.Join(strings.Fields(strings.Join(toks, " ")), " ")
			if len(toks) > 0 && strings.HasPrefix(joined, p.args) {
				return toks
			}
		}
	}
	return strings.Fields(p.args)
}

// topologyForProcess builds topology evidence for one process from its
// arguments and, for llama.cpp only, the allowlisted LLAMA_ARG_RPC variable
// when the operator opted in to reading runtime environments.
func (c *deploymentCollector) topologyForProcess(rt string, p psProc) *Topology {
	in := topologyInput{runtime: rt, tokens: c.argvTokens(p), argvSource: "cmdline", envSource: "env"}
	if rt == "llamacpp" && c.readRuntimeEnv && p.pid > 0 {
		if _, hasArg := flagValue(in.tokens, "--rpc"); !hasArg {
			blob, err := c.readEnviron(p.pid)
			if err != nil {
				in.envUnreadable = true
			} else {
				in.env = topologyEnvList(strings.Split(string(blob), "\x00"))
			}
		}
	}
	return buildTopology(in)
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
	HostConfig struct {
		NetworkMode    string `json:"NetworkMode"`
		DeviceRequests []struct {
			DeviceIDs []string `json:"DeviceIDs"`
		} `json:"DeviceRequests"`
	} `json:"HostConfig"`
}

// dockerHostPort returns the host port to report for a container, or 0 when
// it cannot be known. A --port in the container's arguments is the port inside
// the container, so it counts only when it is published (its host port is
// used) or the container shares the host network. Without such an argument a
// single published port is used; several published ports are ambiguous (one
// may be metrics, not the API), so none is reported rather than a pick that
// could change between refreshes.
func dockerHostPort(argsPort int, cont *dockerContainer) int {
	var published []int
	byContainerPort := map[int][]int{}
	for key, bindings := range cont.NetworkSettings.Ports {
		cport, err := strconv.Atoi(strings.SplitN(key, "/", 2)[0])
		for _, b := range bindings {
			hp, e := strconv.Atoi(b.HostPort)
			if e != nil || hp <= 0 {
				continue
			}
			published = append(published, hp)
			if err == nil {
				byContainerPort[cport] = append(byContainerPort[cport], hp)
			}
		}
	}
	if argsPort > 0 {
		if hps := byContainerPort[argsPort]; len(hps) > 0 {
			sort.Ints(hps)
			return hps[0]
		}
		if cont.HostConfig.NetworkMode == "host" {
			return argsPort
		}
		return 0
	}
	sort.Ints(published)
	for i := 1; i < len(published); i++ {
		if published[i] != published[0] {
			return 0
		}
	}
	if len(published) > 0 {
		return published[0]
	}
	return 0
}

// dockerGPUScope builds the GPU scope for a container. NVIDIA images bake
// NVIDIA_VISIBLE_DEVICES=all into their environment, so that value (or an
// explicit none) says nothing reliable about how this container was started:
// it is reported as unknown, never as "every GPU is visible". Devices named in
// the container's GPU device request (docker run --gpus device=0) are real
// evidence and take precedence over a baked-in NVIDIA variable.
func dockerGPUScope(cont *dockerContainer) (*GPUScope, []int) {
	sc, legacy := scopeFromEnv(allowlistedEnvList(cont.Config.Env), "docker-env:")
	if sc != nil && strings.HasSuffix(sc.Source, ":CUDA_VISIBLE_DEVICES") && (len(sc.Indices) > 0 || len(sc.UUIDs) > 0) {
		return sc, legacy
	}
	var ids []string
	for _, dr := range cont.HostConfig.DeviceRequests {
		ids = append(ids, dr.DeviceIDs...)
	}
	if len(ids) > 0 {
		r, g := parseGPUScope("docker-device-request:", "NVIDIA_VISIBLE_DEVICES", strings.Join(ids, ","))
		return &r, g
	}
	if sc != nil && len(sc.Indices) == 0 && len(sc.UUIDs) == 0 {
		sc.Note = "the container's " + sc.Source[strings.Index(sc.Source, ":")+1:] + " is " + strconv.Quote(sc.Raw) +
			", which can be an image default rather than how the container was started, so which GPUs it uses is not known"
		return sc, nil
	}
	return sc, legacy
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
	port := dockerHostPort(extractPortFromArgs(argsStr), &cont)
	rep := DeploymentReport{
		Runtime:     runtimeHint,
		Port:        port,
		Parallelism: par,
		Caps:        caps,
		Source:      "docker",
	}
	rep.GPUScope, rep.GPUGroup = dockerGPUScope(&cont)
	rep.Topology = buildTopology(topologyInput{
		runtime:    runtimeHint,
		tokens:     append(append([]string(nil), cont.Config.Cmd...), cont.Args...),
		argvSource: "docker-cmd",
		env:        topologyEnvList(cont.Config.Env),
		envSource:  "docker-env",
	})
	return &dockerDeployment{report: rep, pid: cont.State.Pid}
}

// collectFromDockerSocket queries the docker socket if present. Returns nil if
// the socket is absent or no deployments were found (unknown, not fabricated).
func (c *deploymentCollector) collectFromDockerSocket(detected []DetectedRuntime, tree procTree) []DeploymentReport {
	sock := c.dockerSocket
	if _, err := os.Stat(sock); err != nil {
		return nil
	}
	// Each collection builds its own transport, so keep-alive would leave one
	// idle socket (and its goroutines) behind per call. Close every
	// connection when its request finishes instead.
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
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
