package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
