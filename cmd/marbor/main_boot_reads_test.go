package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("warning must be exactly one line, got %d newlines: %q", n, out)
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

func TestLoadDrainStates_UnregisteredNodeIsLoggedAsInfo(t *testing.T) {
	// Arrange
	st := openMemStore(t)
	for name, draining := range map[string]bool{"gone-node": true, "node-a": true, "node-b": false} {
		if err := st.SetNodeDrain(name, draining, "r", 0); err != nil {
			t.Fatal(err)
		}
	}
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadDrainStates(r, st)

	// Assert: only the unregistered drained node is mentioned, and not as a WARNING.
	out := buf.String()
	if !strings.Contains(out, `saved drain state for "gone-node" not applied: no such node is registered`) {
		t.Fatalf("log output = %q, want the unregistered-node line", out)
	}
	if strings.Contains(out, "WARNING") || strings.Contains(out, "node-a") || strings.Contains(out, "node-b") {
		t.Fatalf("log must only name the unregistered node, as information: %q", out)
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

func TestLoadPredictiveHistory_RowsAreSeededAndCounted(t *testing.T) {
	// Arrange
	st := openMemStore(t)
	ts := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := st.AppendPredictiveTransition("m1", "m2", ts.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	r := newBootRouter()
	buf := captureLog(t)

	// Act
	loadPredictiveHistory(r, st)

	// Assert
	out := buf.String()
	if !strings.Contains(out, "store: loaded 3 predictive transition(s)") || strings.Contains(out, "WARNING") {
		t.Fatalf("log output = %q, want the loaded-count line and no warning", out)
	}
}

func TestBootHourLocation(t *testing.T) {
	// 10:00 UTC is 15:30 in Asia/Kolkata (+05:30), 05:00 in America/New_York (-05:00 in January).
	ts := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		tz       string
		wantHour int
		wantWarn bool
	}{
		{"Asia/Kolkata", 15, false},
		{"America/New_York", 5, false},
		{"Local", ts.In(time.Local).Hour(), false},
		{"", ts.In(time.Local).Hour(), false},
		{"Not/AZone", 10, true},
	}
	for _, c := range cases {
		t.Run(c.tz, func(t *testing.T) {
			// Arrange
			buf := captureLog(t)

			// Act
			got := transitionHour(ts, bootHourLocation(c.tz))

			// Assert
			if got != c.wantHour {
				t.Fatalf("hour = %d, want %d", got, c.wantHour)
			}
			if warned := strings.Contains(buf.String(), "WARNING: unknown timezone"); warned != c.wantWarn {
				t.Fatalf("warned = %v, want %v: %q", warned, c.wantWarn, buf.String())
			}
		})
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
		"pinned:node:node-d": `{"a":1}`,
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
	for _, key := range []string{"pinned:node:node-a", "warmup:node:node-b", "pinned:node:node-d"} {
		if !strings.Contains(out, "WARNING: skipped saved setting \""+key+"\"") {
			t.Fatalf("no warning for %s in %q", key, out)
		}
	}
	if strings.Contains(out, "skipped saved setting \"warmup:node:node-c\"") {
		t.Fatalf("good value warned about: %q", out)
	}
	for _, leak := range []string{"SECRETISH", `{"enabled"`, "tru"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log must name the key, never the stored value (found %q): %q", leak, out)
		}
	}
	if nw := r.NodeWarmupSetting("node-c"); !nw.Enabled || len(nw.Models) != 1 {
		t.Fatalf("good warmup value after bad keys was not applied: %+v", nw)
	}
	if nw := r.NodeWarmupSetting("node-b"); nw.Enabled || len(nw.Models) != 0 {
		t.Fatalf("malformed warmup value was applied: %+v", nw)
	}
	for _, n := range []string{"node-a", "node-d"} {
		if got := r.PinnedModels(n); len(got) != 0 {
			t.Fatalf("malformed pinned value applied to %s: %v", n, got)
		}
	}
}

func TestLoadNodeSettings_ValidAndEmptyValuesApplyQuietly(t *testing.T) {
	// Arrange
	st := openMemStore(t)
	for k, v := range map[string]string{
		"pinned:node:node-a":  `["m1","m2"]`,
		"pinned:node:node-b":  ``,
		"pinned:node:node-c":  `[]`,
		"warmup:node:node-a":  `{"enabled":true,"models":["m1"]}`,
		"warmup:node:node-b":  ``,
		"warmup:node:node-c":  `{"enabled":false}`,
		"unrelated:setting":   `not-json`,
		"warmup:node:unknown": `{"enabled":true}`,
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
	if strings.Contains(out, "WARNING") {
		t.Fatalf("unexpected warning: %q", out)
	}
	if !strings.Contains(out, "store: loaded warmup settings for 2 node(s)") {
		t.Fatalf("log output = %q, want the count line for node-a and unknown", out)
	}
	if got := r.PinnedModels("node-a"); len(got) != 2 {
		t.Fatalf("pinned models for node-a = %v, want 2", got)
	}
	for _, n := range []string{"node-b", "node-c"} {
		if got := r.PinnedModels(n); len(got) != 0 {
			t.Fatalf("empty pinned value applied to %s: %v", n, got)
		}
		if nw := r.NodeWarmupSetting(n); nw.Enabled || len(nw.Models) != 0 {
			t.Fatalf("empty or disabled warmup value applied to %s: %+v", n, nw)
		}
	}
	if nw := r.NodeWarmupSetting("node-a"); !nw.Enabled || len(nw.Models) != 1 {
		t.Fatalf("warmup for node-a not applied: %+v", nw)
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
	if strings.Contains(buf.String(), `"id"`) || strings.Contains(buf.String(), "s1") {
		t.Fatalf("log must not include the stored value: %q", buf.String())
	}
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
	if strings.Contains(buf.String(), "WARNING") || !strings.Contains(buf.String(), "store: loaded 1 schedule(s)") {
		t.Fatalf("log output = %q, want the loaded-count line and no warning", buf.String())
	}
	if got := r.Schedules(); len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("schedule not applied: %v", got)
	}
}

func TestLoadSchedules_EmptyValuesStayQuiet(t *testing.T) {
	for _, raw := range []string{"", "[]"} {
		// Arrange
		st := openMemStore(t)
		if err := st.SetSetting("schedules", raw); err != nil {
			t.Fatal(err)
		}
		r := newBootRouter()
		buf := captureLog(t)

		// Act
		loadSchedules(r, st)

		// Assert
		if buf.Len() != 0 || len(r.Schedules()) != 0 {
			t.Fatalf("value %q: log %q schedules %v, want quiet and none", raw, buf.String(), r.Schedules())
		}
	}
}

func TestApplyNodeOverrides_UnknownNodeIsLogged(t *testing.T) {
	// Arrange
	r := newBootRouter()
	vram := int64(1000)
	buf := captureLog(t)

	// Act
	applyNodeOverrides(r, map[string]store.NodeOverride{"gone-node": {VRAMTotalMB: &vram}, "node-a": {VRAMTotalMB: &vram}})

	// Assert: informational, not a WARNING - a removed node's saved row is
	// kept on purpose, so this is expected on every boot after a removal.
	out := buf.String()
	if !strings.Contains(out, `saved override for "gone-node" not applied: no such node is registered`) {
		t.Fatalf("log output = %q, want the unknown-node line", out)
	}
	if strings.Contains(out, "WARNING") || strings.Contains(out, "node-a") {
		t.Fatalf("an orphaned override is expected after a node removal, not a WARNING, and a registered node logs nothing: %q", out)
	}
	if got := projectNode(t, r, "node-a"); got.VRAMTotalMBConfig != 1000 {
		t.Fatalf("registered node-a was not patched alongside the orphan: %+v", got)
	}
}

// main() cannot be unit tested, so this is a deliberate source-text tripwire
// (it needs the package directory as the working directory and breaks on a
// rename): each boot read must stay inside its loader, because an inline read
// that drops the error would bring the silent failure back.
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
