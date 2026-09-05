package control

import (
	"context"
	"fmt"
	"strings"

	"github.com/Anirudhx7/marbor/internal/marboragent/service"
)

// isSelf reports whether a discovered service/container/unit name is the
// Marbor agent's own registration (service.Name, e.g.
// "marbor-agent.service" for systemd) rather than the inference
// runtime it is trying to find. Needed because a naive substring match on
// runtimeName (e.g. "ollama") also matches the agent's own unit name
// ("marbor-agent"), which would make every probe report the agent
// controlling itself instead of the runtime.
func isSelf(name string) bool {
	return strings.Contains(strings.ToLower(name), strings.ToLower(service.Name))
}

// DiscoveryResult is what a re-scan reports for the operator's Accept/Change
// decision - Driver/Identifier are only a suggestion until explicitly
// accepted, and Evidence records what was actually observed, never a bare
// confidence label, so the UI can show the operator why this was suggested.
type DiscoveryResult struct {
	// Driver is empty when nothing above the port-probe fallback resolved -
	// a reachable port alone proves reachability only, never a control
	// method, so it is never used to populate this field.
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

// discoveryCandidate is one substring-matching hit within a single tier's
// probe, before selectBestCandidate's two-pass scoring picks a winner:
// a naive first-match-wins scan lets a decoy unit (e.g. an
// "ollama-backup" service) shadow the real "ollama" unit whenever it happens
// to be listed first.
type discoveryCandidate struct {
	identifier string
	// exactMatch is true when identifier equals runtimeName exactly
	// (case-insensitively, service-manager-suffix-stripped where
	// applicable) rather than merely containing it as a substring.
	exactMatch bool
	active     bool
	evidence   []string
}

// selectBestCandidate scores candidates as: exact-match+active > exact-match
// > substring-match+active > first substring match (the original
// first-match-wins order, kept as the final fallback so a tier still
// resolves something when nothing scores higher).
func selectBestCandidate(candidates []discoveryCandidate) (discoveryCandidate, bool) {
	if len(candidates) == 0 {
		return discoveryCandidate{}, false
	}
	for _, c := range candidates {
		if c.exactMatch && c.active {
			return c, true
		}
	}
	for _, c := range candidates {
		if c.exactMatch {
			return c, true
		}
	}
	for _, c := range candidates {
		if c.active {
			return c, true
		}
	}
	return candidates[0], true
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
	lowerRuntime := strings.ToLower(runtimeName)
	seen := map[string]bool{}
	var candidates []discoveryCandidate

	if out, err := runCommand(ctx, "systemctl", "list-units", "--type=service", "--all", "--no-legend", "--plain"); err == nil {
		for _, line := range splitLines(out) {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			unit := fields[0]
			if isSelf(unit) {
				continue
			}
			lowerUnit := strings.ToLower(unit)
			if !strings.Contains(lowerUnit, lowerRuntime) {
				continue
			}
			seen[lowerUnit] = true
			candidates = append(candidates, discoveryCandidate{
				identifier: unit,
				exactMatch: lowerUnit == lowerRuntime || lowerUnit == lowerRuntime+".service",
				active:     len(fields) >= 3 && fields[2] == "active",
				evidence: []string{
					fmt.Sprintf("unit %q found", unit),
					strings.TrimSpace(line),
				},
			})
		}
	}

	// list-units only sees loaded units, while SystemdDriver.Validate
	// uses list-unit-files and so sees disabled/unloaded units too - union
	// the two here so a stopped-but-installed unit is still discoverable
	// instead of only the currently-loaded ones.
	if out, err := runCommand(ctx, "systemctl", "list-unit-files", "--type=service", "--no-legend", "--plain"); err == nil {
		for _, line := range splitLines(out) {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			unit := fields[0]
			if isSelf(unit) {
				continue
			}
			lowerUnit := strings.ToLower(unit)
			if seen[lowerUnit] || !strings.Contains(lowerUnit, lowerRuntime) {
				continue
			}
			seen[lowerUnit] = true
			candidates = append(candidates, discoveryCandidate{
				identifier: unit,
				exactMatch: lowerUnit == lowerRuntime || lowerUnit == lowerRuntime+".service",
				active:     false,
				evidence: []string{
					fmt.Sprintf("unit file %q found (installed but not currently loaded)", unit),
					strings.TrimSpace(line),
				},
			})
		}
	}

	best, ok := selectBestCandidate(candidates)
	if !ok {
		return DiscoveryResult{}, false
	}
	return DiscoveryResult{Driver: "systemd", Identifier: best.identifier, Evidence: best.evidence}, true
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
	lowerRuntime := strings.ToLower(runtimeName)
	var candidates []discoveryCandidate
	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		label := fields[len(fields)-1]
		if isSelf(label) {
			continue
		}
		lowerLabel := strings.ToLower(label)
		if !strings.Contains(lowerLabel, lowerRuntime) {
			continue
		}
		// launchctl list's PID column is "-" when the job is loaded but not
		// currently running.
		candidates = append(candidates, discoveryCandidate{
			identifier: label,
			exactMatch: lowerLabel == lowerRuntime,
			active:     fields[0] != "-",
			evidence: []string{
				fmt.Sprintf("launchd label %q found", label),
				strings.TrimSpace(line),
			},
		})
	}
	best, ok := selectBestCandidate(candidates)
	if !ok {
		return DiscoveryResult{}, false
	}
	return DiscoveryResult{Driver: "launchd", Identifier: best.identifier, Evidence: best.evidence}, true
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
	lowerRuntime := strings.ToLower(runtimeName)
	var candidates []discoveryCandidate

	// "sc query" output is a sequence of per-service blocks, each starting
	// with a SERVICE_NAME line and (a few lines later) a STATE line - walk
	// the blocks so active state can be scored, not just presence.
	var curName string
	var curMatch bool
	flush := func(running bool) {
		if curName == "" || !curMatch {
			curName = ""
			return
		}
		lowerName := strings.ToLower(curName)
		candidates = append(candidates, discoveryCandidate{
			identifier: curName,
			exactMatch: lowerName == lowerRuntime,
			active:     running,
			evidence:   []string{fmt.Sprintf("service %q found", curName)},
		})
		curName = ""
	}
	for _, line := range splitLines(out) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "SERVICE_NAME:") {
			flush(false) // previous block never showed a STATE line
			curName = strings.TrimSpace(strings.TrimPrefix(trimmed, "SERVICE_NAME:"))
			curMatch = !isSelf(curName) && strings.Contains(strings.ToLower(curName), lowerRuntime)
			continue
		}
		if strings.HasPrefix(trimmed, "STATE") && curName != "" {
			flush(strings.Contains(trimmed, "RUNNING"))
		}
	}
	flush(false)

	best, ok := selectBestCandidate(candidates)
	if !ok {
		return DiscoveryResult{}, false
	}
	return DiscoveryResult{Driver: "windows_service", Identifier: best.identifier, Evidence: best.evidence}, true
}

// dockerProber lists containers (running and stopped) and looks for one
// whose name or image contains runtimeName.
type dockerProber struct{}

func (dockerProber) probe(ctx context.Context, runtimeName string) (DiscoveryResult, bool) {
	if _, err := lookPath("docker"); err != nil {
		return DiscoveryResult{}, false
	}
	out, err := runCommand(ctx, "docker", "ps", "-a", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}")
	if err != nil {
		return DiscoveryResult{}, false
	}
	lower := strings.ToLower(runtimeName)
	var candidates []discoveryCandidate
	for _, line := range splitLines(out) {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 0 {
			continue
		}
		name := parts[0]
		var image, status string
		if len(parts) > 1 {
			image = parts[1]
		}
		if len(parts) > 2 {
			status = parts[2]
		}
		if isSelf(name) {
			continue
		}
		lowerName := strings.ToLower(name)
		if !strings.Contains(lowerName, lower) && !strings.Contains(strings.ToLower(image), lower) {
			continue
		}
		evidence := []string{fmt.Sprintf("docker container %q found", name)}
		if image != "" {
			evidence = append(evidence, fmt.Sprintf("image %s", image))
		}
		candidates = append(candidates, discoveryCandidate{
			identifier: name,
			exactMatch: lowerName == lower,
			// docker ps Status starts with "Up" for a running container,
			// "Exited"/"Created" otherwise.
			active:   strings.HasPrefix(status, "Up"),
			evidence: evidence,
		})
	}
	best, ok := selectBestCandidate(candidates)
	if !ok {
		return DiscoveryResult{}, false
	}
	return DiscoveryResult{Driver: "docker", Identifier: best.identifier, Evidence: best.evidence}, true
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
			if !processAlive(pid) {
				continue
			}
			return DiscoveryResult{
				Driver:     "process",
				Identifier: path,
				Evidence:   []string{fmt.Sprintf("pid file %q found (pid %d, confirmed alive)", path, pid)},
			}, true
		}
	}
	return DiscoveryResult{}, false
}
