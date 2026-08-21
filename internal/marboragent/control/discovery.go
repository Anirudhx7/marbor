package control

import (
	"context"
	"fmt"
	"strings"

	"github.com/ollama-mesh/ollama-mesh/internal/marboragent/service"
)

// isSelf reports whether a discovered service/container/unit name is the
// node agent's own registration (service.Name, e.g.
// "marbor-agent.service" for systemd) rather than the inference
// runtime it is trying to find. Needed because a naive substring match on
// runtimeName (e.g. "ollama") also matches the agent's own unit name
// ("marbor-agent"), which would make every probe report the agent
// controlling itself instead of the runtime.
func isSelf(name string) bool {
	return strings.Contains(strings.ToLower(name), strings.ToLower(service.Name))
}

// DiscoveryResult is what a re-scan reports for the operator's Accept/Change
// decision (node-agent-capabilities.md section 5.5) - Driver/Identifier are
// only a suggestion until explicitly accepted (section 5.6), and Evidence
// records what was actually observed, never a bare confidence label, so the
// UI can show the operator why this was suggested.
type DiscoveryResult struct {
	// Driver is empty when nothing above the port-probe fallback resolved -
	// a reachable port alone proves reachability only, never a control
	// method (section 5.3), so it is never used to populate this field.
	Driver     string
	Identifier string
	Evidence   []string
}

// prober is one tier of the confidence-ordered discovery sequence (5.3).
// Each tier's probe is independent of the others - discovery stops at the
// first tier that finds something, in order.
type prober interface {
	probe(ctx context.Context, runtimeName string) (DiscoveryResult, bool)
}

// Discover runs the confidence-ordered probe sequence: service manager (any
// of systemd/launchd/windows_service - whichever tool is actually present
// on this host) -> Docker container inspect -> PID file convention -> port
// probe last (reachability only, never selects a driver). runtimeName is
// the already-detected inference runtime (e.g. "ollama"), used as the
// substring each tier searches for since the control layer has no other way
// to know which unit/container/process belongs to it. reachableURL, when
// non-empty, is the base URL the runtime answered on (from
// RuntimeDetector), used only for the port-probe evidence line, never to
// pick a driver.
func Discover(ctx context.Context, runtimeName, reachableURL string) DiscoveryResult {
	tiers := []prober{
		systemdProber{}, launchdProber{}, windowsServiceProber{},
		dockerProber{},
		processProber{},
	}
	for _, t := range tiers {
		if res, found := t.probe(ctx, runtimeName); found {
			return res
		}
	}
	if reachableURL != "" {
		return DiscoveryResult{
			Evidence: []string{fmt.Sprintf("reachable at %s (reachability only, no control method identified)", reachableURL)},
		}
	}
	return DiscoveryResult{}
}

// systemdProber lists systemd units and looks for one whose name contains
// runtimeName.
type systemdProber struct{}

func (systemdProber) probe(ctx context.Context, runtimeName string) (DiscoveryResult, bool) {
	if _, err := lookPath("systemctl"); err != nil {
		return DiscoveryResult{}, false
	}
	out, err := runCommand(ctx, "systemctl", "list-units", "--type=service", "--all", "--no-legend", "--plain")
	if err != nil {
		return DiscoveryResult{}, false
	}
	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		unit := fields[0]
		if isSelf(unit) {
			continue
		}
		if strings.Contains(strings.ToLower(unit), strings.ToLower(runtimeName)) {
			return DiscoveryResult{
				Driver:     "systemd",
				Identifier: unit,
				Evidence: []string{
					fmt.Sprintf("unit %q found", unit),
					strings.TrimSpace(line),
				},
			}, true
		}
	}
	return DiscoveryResult{}, false
}

// launchdProber lists loaded launchd jobs and looks for one whose label
// contains runtimeName.
type launchdProber struct{}

func (launchdProber) probe(ctx context.Context, runtimeName string) (DiscoveryResult, bool) {
	if _, err := lookPath("launchctl"); err != nil {
		return DiscoveryResult{}, false
	}
	out, err := runCommand(ctx, "launchctl", "list")
	if err != nil {
		return DiscoveryResult{}, false
	}
	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		label := fields[len(fields)-1]
		if isSelf(label) {
			continue
		}
		if strings.Contains(strings.ToLower(label), strings.ToLower(runtimeName)) {
			return DiscoveryResult{
				Driver:     "launchd",
				Identifier: label,
				Evidence: []string{
					fmt.Sprintf("launchd label %q found", label),
					strings.TrimSpace(line),
				},
			}, true
		}
	}
	return DiscoveryResult{}, false
}

// windowsServiceProber lists Windows services and looks for one whose name
// contains runtimeName.
type windowsServiceProber struct{}

func (windowsServiceProber) probe(ctx context.Context, runtimeName string) (DiscoveryResult, bool) {
	if _, err := lookPath("sc"); err != nil {
		return DiscoveryResult{}, false
	}
	out, err := runCommand(ctx, "sc", "query", "type=", "service", "state=", "all")
	if err != nil {
		return DiscoveryResult{}, false
	}
	for _, line := range splitLines(out) {
		if !strings.HasPrefix(strings.TrimSpace(line), "SERVICE_NAME:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "SERVICE_NAME:"))
		if isSelf(name) {
			continue
		}
		if strings.Contains(strings.ToLower(name), strings.ToLower(runtimeName)) {
			return DiscoveryResult{
				Driver:     "windows_service",
				Identifier: name,
				Evidence:   []string{fmt.Sprintf("service %q found", name)},
			}, true
		}
	}
	return DiscoveryResult{}, false
}

// dockerProber lists containers (running and stopped) and looks for one
// whose name or image contains runtimeName.
type dockerProber struct{}

func (dockerProber) probe(ctx context.Context, runtimeName string) (DiscoveryResult, bool) {
	if _, err := lookPath("docker"); err != nil {
		return DiscoveryResult{}, false
	}
	out, err := runCommand(ctx, "docker", "ps", "-a", "--format", "{{.Names}}\t{{.Image}}")
	if err != nil {
		return DiscoveryResult{}, false
	}
	lower := strings.ToLower(runtimeName)
	for _, line := range splitLines(out) {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 0 {
			continue
		}
		name := parts[0]
		image := ""
		if len(parts) > 1 {
			image = parts[1]
		}
		if isSelf(name) {
			continue
		}
		if strings.Contains(strings.ToLower(name), lower) || strings.Contains(strings.ToLower(image), lower) {
			evidence := []string{fmt.Sprintf("docker container %q found", name)}
			if image != "" {
				evidence = append(evidence, fmt.Sprintf("image %s", image))
			}
			return DiscoveryResult{Driver: "docker", Identifier: name, Evidence: evidence}, true
		}
	}
	return DiscoveryResult{}, false
}

// processCandidatePIDFiles are the conventional PID file locations checked
// for a bare (non-service, non-container) process, in order.
var processCandidatePIDFiles = func(runtimeName string) []string {
	return []string{
		"/var/run/" + runtimeName + ".pid",
		"/run/" + runtimeName + ".pid",
	}
}

// processProber checks conventional PID file paths for runtimeName.
type processProber struct{}

func (processProber) probe(ctx context.Context, runtimeName string) (DiscoveryResult, bool) {
	if runtimeName == "" {
		return DiscoveryResult{}, false
	}
	for _, path := range processCandidatePIDFiles(runtimeName) {
		if pid, err := readPIDFile(path); err == nil {
			return DiscoveryResult{
				Driver:     "process",
				Identifier: path,
				Evidence:   []string{fmt.Sprintf("pid file %q found (pid %d)", path, pid)},
			}, true
		}
	}
	return DiscoveryResult{}, false
}
