package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

func blockedFleet(t *testing.T) (*Server, *router.Router) {
	t.Helper()
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "head", URL: "http://head:11434", Runtime: "vllm"},
		{Name: "wa", URL: "http://wa:11434", Runtime: "vllm"},
		{Name: "wb", URL: "http://wb:11434", Runtime: "vllm"},
		{Name: "solo", URL: "http://solo:11434", Runtime: "vllm"},
	}, nil)
	s := NewServer(r, nil, config.Config{})
	for _, name := range []string{"head", "wa", "wb"} {
		req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/"+name,
			strings.NewReader(`{"replica_peers":{"members":["head","wa","wb"],"head":"head"}}`))
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		s.handlePatchNode(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
	}
	return s, r
}

func setStale(r *router.Router, name string, stale bool) {
	for _, n := range r.Nodes() {
		if n.Name == name {
			n.Lock()
			n.AgentStale = stale
			n.Unlock()
		}
	}
}

func getNode(t *testing.T, s *Server, name string) (nodeResp, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/"+name, nil)
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	s.handleNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var n nodeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	return n, rec.Body.String()
}

func TestNodes_ReplicaBlockedByOnHeadOnly(t *testing.T) {
	s, r := blockedFleet(t)
	setStale(r, "wb", true)
	setStale(r, "wa", true)
	_, body := getNodes(t, s, "")
	byName := map[string]nodeResp{}
	for _, n := range decodeNodes(t, body) {
		byName[n.Name] = n
	}
	if got := byName["head"].ReplicaBlockedBy; got != "wa" {
		t.Errorf("head replicaBlockedBy = %q, want wa (lowest-named silent member)", got)
	}
	for _, n := range []string{"wa", "wb", "solo"} {
		if byName[n].ReplicaBlockedBy != "" {
			t.Errorf("%s carries replicaBlockedBy %q, want none", n, byName[n].ReplicaBlockedBy)
		}
	}
	if byName["head"].Health != "healthy" {
		t.Errorf("head health = %q, want healthy (not a verdict on the head)", byName["head"].Health)
	}
	if got := byName["head"].Signals; len(got) != 1 || got[0] != "healthy" {
		t.Errorf("head signals = %v, want [healthy]", got)
	}
}

func TestNodes_ReplicaBlockedByAbsentWhenNothingStale(t *testing.T) {
	s, r := blockedFleet(t)
	_, body := getNodes(t, s, "")
	if strings.Contains(string(body), "replicaBlockedBy") {
		t.Fatalf("key present with no stale member: %s", body)
	}
	_, single := getNode(t, s, "head")
	if strings.Contains(single, "replicaBlockedBy") {
		t.Fatalf("single-node key present with no stale member: %s", single)
	}
	setStale(r, "wa", true)
	setStale(r, "wa", false)
	_, body = getNodes(t, s, "")
	if strings.Contains(string(body), "replicaBlockedBy") {
		t.Fatalf("key still present after the agent came back: %s", body)
	}
}

func TestNode_ReplicaBlockedByMatchesList(t *testing.T) {
	s, r := blockedFleet(t)
	setStale(r, "wb", true)
	head, _ := getNode(t, s, "head")
	if head.ReplicaBlockedBy != "wb" {
		t.Errorf("head via handleNode = %q, want wb", head.ReplicaBlockedBy)
	}
	wb, _ := getNode(t, s, "wb")
	if wb.ReplicaBlockedBy != "" {
		t.Errorf("worker via handleNode = %q, want none", wb.ReplicaBlockedBy)
	}
}

func TestNodes_ReplicaBlockedByCarriedOnFilteredList(t *testing.T) {
	s, r := blockedFleet(t)
	setStale(r, "wb", true)
	for _, n := range r.Nodes() {
		if n.Name == "wb" {
			n.Lock()
			n.Healthy = false
			n.Unlock()
		}
	}
	// The list is filtered to the head only, the silent worker is omitted.
	_, body := getNodes(t, s, "signal=healthy")
	var head *nodeResp
	for _, n := range decodeNodes(t, body) {
		if n.Name == "wb" {
			t.Fatal("worker unexpectedly in the healthy filter")
		}
		if n.Name == "head" {
			n := n
			head = &n
		}
	}
	if head == nil || head.ReplicaBlockedBy != "wb" {
		t.Fatalf("filtered head = %+v, want replicaBlockedBy wb", head)
	}
}

func TestNodes_ReplicaBlockedByReportedForDownOrDrainingHead(t *testing.T) {
	s, r := blockedFleet(t)
	setStale(r, "wa", true)
	for _, n := range r.Nodes() {
		if n.Name == "head" {
			n.Lock()
			n.Healthy = false
			n.Draining = true
			n.Unlock()
		}
	}
	head, _ := getNode(t, s, "head")
	if head.ReplicaBlockedBy != "wa" {
		t.Errorf("API must report the fact regardless of head health, got %q", head.ReplicaBlockedBy)
	}
}

func TestNodes_ReplicaBlockedByRace(t *testing.T) {
	s, r := blockedFleet(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			setStale(r, "wa", i%2 == 0)
			time.Sleep(200 * time.Microsecond)
		}
	}()
	for i := 0; i < 30; i++ {
		getNodes(t, s, "")
		getNode(t, s, "head")
	}
	close(stop)
	wg.Wait()
}
