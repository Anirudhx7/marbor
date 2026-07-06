package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRunSchedulesWarmupFiresOncePerMinute verifies a warmup schedule fires at
// its matching HH:MM and does not double-fire within the same minute.
func TestRunSchedulesWarmupFiresOncePerMinute(t *testing.T) {
	calls := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes:          []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		schedLastFired: map[string]string{},
	}
	now := time.Now()
	at := fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute())
	r.SetSchedules([]Schedule{{ID: "s1", Action: "warmup", Node: "n1", Models: []string{"llama3"}, At: at, Enabled: true}})

	r.runSchedules(context.Background(), now)
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled warmup did not fire at matching time")
	}

	// Same minute again: must NOT fire a second time.
	r.runSchedules(context.Background(), now)
	select {
	case <-calls:
		t.Error("scheduled warmup fired twice within the same minute")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRunSchedulesSkipsNonMatchingTime verifies a schedule does not fire when
// the current time doesn't match its At.
func TestRunSchedulesSkipsNonMatchingTime(t *testing.T) {
	calls := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes:          []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		schedLastFired: map[string]string{},
	}
	now := time.Now()
	other := now.Add(2 * time.Minute)
	at := fmt.Sprintf("%02d:%02d", other.Hour(), other.Minute())
	r.SetSchedules([]Schedule{{ID: "s1", Action: "warmup", Node: "n1", Models: []string{"llama3"}, At: at, Enabled: true}})

	r.runSchedules(context.Background(), now)
	select {
	case <-calls:
		t.Error("schedule fired at a non-matching time")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRunSchedulesDisabledDoesNotFire verifies a disabled schedule is skipped.
func TestRunSchedulesDisabledDoesNotFire(t *testing.T) {
	calls := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes:          []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		schedLastFired: map[string]string{},
	}
	now := time.Now()
	at := fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute())
	r.SetSchedules([]Schedule{{ID: "s1", Action: "warmup", Node: "n1", Models: []string{"llama3"}, At: at, Enabled: false}})

	r.runSchedules(context.Background(), now)
	select {
	case <-calls:
		t.Error("disabled schedule fired")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRunSchedulesDrainFires verifies a "drain" schedule actually sets the
// node's Draining flag when it matches, exercising the full ticker-driven
// dispatch path rather than calling DrainNode directly.
func TestRunSchedulesDrainFires(t *testing.T) {
	r := &Router{
		nodes:          []*NodeState{{Name: "n1", Healthy: true}},
		schedLastFired: map[string]string{},
	}
	now := time.Now()
	at := fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute())
	r.SetSchedules([]Schedule{{ID: "s1", Action: "drain", Node: "n1", At: at, Enabled: true}})

	r.runSchedules(context.Background(), now)

	if !r.nodes[0].Draining {
		t.Error("scheduled drain did not set Draining=true")
	}
}

// TestRunSchedulesUndrainFires verifies an "undrain" schedule clears the
// node's Draining flag when it matches.
func TestRunSchedulesUndrainFires(t *testing.T) {
	r := &Router{
		nodes:          []*NodeState{{Name: "n1", Healthy: true, Draining: true}},
		schedLastFired: map[string]string{},
	}
	now := time.Now()
	at := fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute())
	r.SetSchedules([]Schedule{{ID: "s1", Action: "undrain", Node: "n1", At: at, Enabled: true}})

	r.runSchedules(context.Background(), now)

	if r.nodes[0].Draining {
		t.Error("scheduled undrain did not clear Draining")
	}
}

// TestRunSchedulesUnloadFires verifies an "unload" schedule sends a
// keep_alive:0 request for each configured model, exercising the full
// ticker-driven dispatch path (runSchedules -> fireSchedule -> UnloadModels).
func TestRunSchedulesUnloadFires(t *testing.T) {
	calls := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls <- string(b)
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes:          []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		schedLastFired: map[string]string{},
	}
	now := time.Now()
	at := fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute())
	r.SetSchedules([]Schedule{{ID: "s1", Action: "unload", Node: "n1", Models: []string{"llama3"}, At: at, Enabled: true}})

	r.runSchedules(context.Background(), now)
	select {
	case body := <-calls:
		if !strings.Contains(body, `"keep_alive":0`) || !strings.Contains(body, `"llama3"`) {
			t.Errorf("unload body missing keep_alive:0 or model: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled unload did not fire at matching time")
	}
}

// TestRunSchedulesUnknownNodeDoesNotPanic verifies that a schedule pointed at
// a node that no longer exists (renamed/removed after the schedule was
// created) is a harmless no-op for every action type, instead of panicking
// or silently claiming success with no side effect at all going unnoticed.
func TestRunSchedulesUnknownNodeDoesNotPanic(t *testing.T) {
	for _, action := range []string{"warmup", "unload", "drain", "undrain"} {
		r := &Router{
			nodes:          []*NodeState{{Name: "n1", Healthy: true}},
			schedLastFired: map[string]string{},
		}
		now := time.Now()
		at := fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute())
		r.SetSchedules([]Schedule{{ID: "s1", Action: action, Node: "ghost", Models: []string{"llama3"}, At: at, Enabled: true}})

		r.runSchedules(context.Background(), now)
		time.Sleep(50 * time.Millisecond)

		if r.nodes[0].Draining {
			t.Errorf("action %q against unknown node affected unrelated node n1", action)
		}
	}
}
