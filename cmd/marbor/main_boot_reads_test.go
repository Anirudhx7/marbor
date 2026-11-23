package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

// Boot-time reads of persisted state must log a WARNING when they fail or
// when a stored value cannot be parsed, so an operator can tell that saved
// state was not applied. These tests share captureLog and openMemStore with
// main_node_overrides_test.go, so none of them may call t.Parallel.

var errInjectedRead = errors.New("injected read failure")

type failingDrainRead struct{ store.Store }

func (failingDrainRead) NodeDrainStates() (map[string]store.NodeDrainState, error) {
	return map[string]store.NodeDrainState{"node-a": {Draining: true, Reason: "thermal"}}, errInjectedRead
}

type failingHistoryRead struct{ store.Store }

func (failingHistoryRead) PredictiveHistory() ([]store.PredictiveTransition, error) {
	return nil, errInjectedRead
}

type failingSettingsRead struct{ store.Store }

func (failingSettingsRead) AllSettings() (map[string]string, error) {
	return map[string]string{"pinned:node:node-a": `["m1"]`}, errInjectedRead
}

type failingScheduleRead struct{ store.Store }

func (failingScheduleRead) GetSetting(string) (string, error) {
	return `[{"id":"s1","action":"drain","node":"node-a","at":"01:00","enabled":true}]`, errInjectedRead
}

func requireOneWarningLine(t *testing.T, out string, wantParts ...string) {
	t.Helper()
	for _, part := range wantParts {
		if !strings.Contains(out, part) {
			t.Fatalf("log output = %q, want it to contain %q", out, part)
		}
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("warning must be exactly one line: %q", out)
	}
}

func TestLoadDrainStates_ReadErrorIsLoggedAndNothingDrained(t *testing.T) {
	// Arrange
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadDrainStates(r, failingDrainRead{openMemStore(t)})

	// Assert
	requireOneWarningLine(t, buf.String(),
		"WARNING: could not load node drain states from store: injected read failure",
		"were not re-drained")
	for _, n := range r.Nodes() {
		if n.Draining {
			t.Fatalf("node %s drained from a failed read", n.Name)
		}
	}
}

func TestLoadDrainStates_SuccessRestoresAndStaysQuiet(t *testing.T) {
	// Arrange
	st := openMemStore(t)
	if err := st.SetNodeDrain("node-a", true, "thermal", 120); err != nil {
		t.Fatal(err)
	}
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadDrainStates(r, st)

	// Assert
	if buf.Len() != 0 {
		t.Fatalf("unexpected log output on a good read: %q", buf.String())
	}
	for _, n := range r.Nodes() {
		if n.Name == "node-a" && (!n.Draining || n.DrainedReason != "thermal" || n.DrainedGraceSeconds != 120) {
			t.Fatalf("node-a drain not restored: %+v", n)
		}
		if n.Name == "node-b" && n.Draining {
			t.Fatal("node-b drained without a row")
		}
	}
}

func TestLoadPredictiveHistory_ReadErrorIsLogged(t *testing.T) {
	// Arrange
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadPredictiveHistory(r, failingHistoryRead{openMemStore(t)})

	// Assert
	requireOneWarningLine(t, buf.String(),
		"WARNING: could not load predictive history from store: injected read failure")
}

func TestLoadPredictiveHistory_EmptyStaysQuiet(t *testing.T) {
	// Arrange
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadPredictiveHistory(r, openMemStore(t))

	// Assert
	if buf.Len() != 0 {
		t.Fatalf("unexpected log output for an empty history: %q", buf.String())
	}
}

func TestLoadNodeSettings_ReadErrorIsLoggedAndNothingApplied(t *testing.T) {
	// Arrange
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadNodeSettings(r, failingSettingsRead{openMemStore(t)})

	// Assert
	requireOneWarningLine(t, buf.String(),
		"WARNING: could not load saved settings from store: injected read failure",
		"were not applied")
	if got := r.PinnedModels("node-a"); len(got) != 0 {
		t.Fatalf("pinned models applied from a failed read: %v", got)
	}
}

func TestLoadNodeSettings_MalformedValuesAreLoggedByKeyOnly(t *testing.T) {
	// Arrange
	st := openMemStore(t)
	for k, v := range map[string]string{
		"pinned:node:node-a": `not-json-SECRETISH`,
		"warmup:node:node-b": `{"enabled": tru`,
		"warmup:node:node-c": `{"enabled":true,"models":["m1"]}`,
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadNodeSettings(r, st)

	// Assert
	out := buf.String()
	for _, key := range []string{"pinned:node:node-a", "warmup:node:node-b"} {
		if !strings.Contains(out, "WARNING: skipped saved setting \""+key+"\"") {
			t.Fatalf("no warning for %s in %q", key, out)
		}
	}
	if strings.Contains(out, "skipped saved setting \"warmup:node:node-c\"") {
		t.Fatalf("good value warned about: %q", out)
	}
	if strings.Contains(out, "SECRETISH") || strings.Contains(out, "tru`") {
		t.Fatalf("log must name the key, never the stored value: %q", out)
	}
}

func TestLoadSchedules_ReadErrorIsLoggedAndNothingApplied(t *testing.T) {
	// Arrange
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadSchedules(r, failingScheduleRead{openMemStore(t)})

	// Assert
	requireOneWarningLine(t, buf.String(),
		"WARNING: could not load schedules from store: injected read failure",
		"were not applied")
	if got := r.Schedules(); len(got) != 0 {
		t.Fatalf("schedules applied from a failed read: %v", got)
	}
}

func TestLoadSchedules_MalformedIsLoggedWithoutValue(t *testing.T) {
	// Arrange
	st := openMemStore(t)
	if err := st.SetSetting("schedules", `[{"id":"s1",`); err != nil {
		t.Fatal(err)
	}
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadSchedules(r, st)

	// Assert
	requireOneWarningLine(t, buf.String(), "WARNING: skipped saved schedules: stored value is not valid JSON")
	if got := r.Schedules(); len(got) != 0 {
		t.Fatalf("schedules applied from a malformed value: %v", got)
	}
}

func TestLoadSchedules_SuccessAppliesAndLogsCount(t *testing.T) {
	// Arrange
	st := openMemStore(t)
	if err := st.SetSetting("schedules", `[{"id":"s1","action":"drain","node":"node-a","at":"01:00","enabled":true}]`); err != nil {
		t.Fatal(err)
	}
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadSchedules(r, st)

	// Assert
	if strings.Contains(buf.String(), "WARNING") {
		t.Fatalf("unexpected warning on a good read: %q", buf.String())
	}
	if got := r.Schedules(); len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("schedule not applied: %v", got)
	}
}

func TestApplyNodeOverrides_UnknownNodeIsLogged(t *testing.T) {
	// Arrange
	r := newBootRouter()
	vram := int64(1000)
	buf := captureLog(t)

	// Act
	applyNodeOverrides(r, map[string]store.NodeOverride{"gone-node": {VRAMTotalMB: &vram}})

	// Assert: informational, not a WARNING - a removed node's saved row is
	// kept on purpose, so this is expected on every boot after a removal.
	out := buf.String()
	if !strings.Contains(out, `saved override for "gone-node" not applied: no such node is registered`) {
		t.Fatalf("log output = %q, want the unknown-node line", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Fatalf("an orphaned override is expected after a node removal, not a WARNING: %q", out)
	}
}

// main() cannot be unit tested, so this pins each boot read to its loader:
// an inline read that drops the error would bring the silent failure back.
func TestBootReadsGoThroughLoaders(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, read := range []string{"st.NodeDrainStates()", "st.PredictiveHistory()", "st.AllSettings()", `st.GetSetting("schedules")`} {
		if n := strings.Count(text, read); n != 1 {
			t.Errorf("main.go reads %s %d times, want exactly 1 (inside its loader)", read, n)
		}
	}
	for _, call := range []string{"\tloadDrainStates(r, st)\n", "\tloadPredictiveHistory(r, st)\n", "\tloadNodeSettings(r, st)\n", "\tloadSchedules(r, st)\n"} {
		if !strings.Contains(text, call) {
			t.Errorf("main.go boot no longer calls %q", strings.TrimSpace(call))
		}
	}
}
