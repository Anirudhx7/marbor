package admin

import (
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestNodeSignals_StalledIsAnOverlay(t *testing.T) {
	tests := []struct {
		name string
		resp nodeResp
		want []string
	}{
		{"stalled keeps healthy", nodeResp{Health: "healthy", Stalled: true}, []string{"healthy", "stalled"}},
		{"stalled and draining", nodeResp{Health: "healthy", Draining: true, Stalled: true}, []string{"draining", "stalled"}},
		{"below threshold is not stalled", nodeResp{Health: "healthy", ConsecutiveStalls: 2}, []string{"healthy"}},
		{"stalled comes last", nodeResp{Health: "healthy", AgentPresent: true, Stalled: true}, []string{"healthy", "agent", "stalled"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeSignals(tt.resp); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func stalledFleet(t *testing.T) (*Server, *router.Router) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "stalled.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "a-ok", URL: "http://a:11434", Runtime: "ollama"},
		{Name: "b-stalled", URL: "http://b:11434", Runtime: "vllm"},
		{Name: "c-one-stall", URL: "http://c:11434", Runtime: "llamacpp"},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
	}
	for _, id := range []string{"r1", "r2", "r3"} {
		r.RecordStall("b-stalled", router.StallPhaseFirstByte, id)
	}
	r.RecordStall("c-one-stall", router.StallPhaseFirstByte, "r1")
	return NewServer(r, nil, config.Config{}, st), r
}

func TestHandleNodes_StalledSignalAndFields(t *testing.T) {
	s, _ := stalledFleet(t)
	code, body := getNodes(t, s, "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	byName := map[string]nodeResp{}
	for _, n := range decodeNodes(t, body) {
		byName[n.Name] = n
	}
	b := byName["b-stalled"]
	if !b.Stalled || b.ConsecutiveStalls != 3 || b.StallPhase != "first_byte" || b.LastStallAt == "" {
		t.Fatalf("b-stalled fields wrong: %+v", b)
	}
	if !reflect.DeepEqual(b.Signals, []string{"healthy", "stalled"}) {
		t.Fatalf("b-stalled signals = %v, want healthy to stay and stalled appended", b.Signals)
	}
	c := byName["c-one-stall"]
	if c.Stalled || c.ConsecutiveStalls != 1 || !reflect.DeepEqual(c.Signals, []string{"healthy"}) {
		t.Fatalf("one stall must show evidence but no verdict: %+v", c)
	}
	a := byName["a-ok"]
	if a.Stalled || a.ConsecutiveStalls != 0 || a.LastStallAt != "" || a.StallPhase != "" {
		t.Fatalf("a node with no record must carry no stall data: %+v", a)
	}
}

func TestHandleNodes_StalledFieldsOmittedWhenUnknown(t *testing.T) {
	s, _ := stalledFleet(t)
	_, body := getNodes(t, s, "q=a-ok")
	for _, key := range []string{"stalled", "consecutiveStalls", "lastStallAt", "stallPhase"} {
		if strings.Contains(string(body), `"`+key+`"`) {
			t.Errorf("%q must be omitted for a node with no stall record: %s", key, body)
		}
	}
}

func TestHandleNodes_SignalStalledFilter(t *testing.T) {
	s, _ := stalledFleet(t)
	code, body := getNodes(t, s, "signal=stalled")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	if got := namesOf(decodeNodes(t, body)); !reflect.DeepEqual(got, []string{"b-stalled"}) {
		t.Fatalf("got %v, want only b-stalled", got)
	}
	// healthy is unchanged: it still includes the stalled node.
	_, body = getNodes(t, s, "signal=healthy")
	if got := namesOf(decodeNodes(t, body)); len(got) != 3 {
		t.Fatalf("healthy must keep its meaning, got %v", got)
	}
}

func TestHandleNodes_UnknownSignalMessageListsStalled(t *testing.T) {
	s, _ := stalledFleet(t)
	code, body := getNodes(t, s, "signal=bogus")
	if code != http.StatusBadRequest {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(string(body), "pressure, stalled") {
		t.Fatalf("error must list the new id after pressure: %s", body)
	}
}
