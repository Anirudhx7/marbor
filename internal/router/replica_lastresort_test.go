package router

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

const lastResortLogNeedle = "as a last resort"

func lastResortLogLines(buf *bytes.Buffer) int {
	return strings.Count(buf.String(), lastResortLogNeedle)
}

func headWithStaleWorker(t *testing.T) *Router {
	t.Helper()
	cfgs := []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8000", Host: "h0"},
		{Name: "worker", URL: "http://worker.invalid:8000", Host: "h1"},
	}
	r := replicaStaleRouter(t, cfgs, "head", "worker")
	setStaleByName(t, r, "worker", true)
	return r
}

func TestLastResortLogLimitSurvivesPollPrune(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	key := lastResortLogPrefix + "head@h0"
	if !r.allowHostLog(key) {
		t.Fatal("first call must be allowed")
	}
	// The host of the head has no polled agent, so it is absent from the groups.
	r.dropHostEvidenceNotIn(map[string][]*NodeState{})
	if r.allowHostLog(key) {
		t.Error("the poll prune reset the last-resort log limit for a host without an agent")
	}
	r.hostEvidenceMu.Lock()
	r.hostLogAt[key] = time.Now().Add(-hostLogInterval - time.Second)
	r.hostEvidenceMu.Unlock()
	r.dropHostEvidenceNotIn(map[string][]*NodeState{})
	r.hostEvidenceMu.RLock()
	_, still := r.hostLogAt[key]
	r.hostEvidenceMu.RUnlock()
	if still {
		t.Error("an aged-out last-resort key must be pruned")
	}
}

func TestLastResortRouteLogsOnceThenRateLimited(t *testing.T) {
	r := headWithStaleWorker(t)
	buf := captureLog(t)

	for i := 0; i < 3; i++ {
		picked, _, dec := r.Route("", "", "")
		if picked == nil || picked.Name != "head" || dec == nil || !strings.Contains(dec.Detail, "last resort") {
			t.Fatalf("route %d: picked %v decision %+v, want head as last resort", i, picked, dec)
		}
		// A poll pass in between must not reset the limit.
		r.dropHostEvidenceNotIn(map[string][]*NodeState{})
	}
	if n := lastResortLogLines(buf); n != 1 {
		t.Fatalf("logged %d last-resort lines over 3 routes, want 1:\n%s", n, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"\"head\"", "\"worker\"", "\"h1\""} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q should name the head, the dark member and its host (missing %s)", out, want)
		}
	}
}

func TestLastResortLogIsPerHeadNotPerHost(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8000", Host: "same"},
		{Name: "worker", URL: "http://worker.invalid:8000", Host: "h1"},
		{Name: "head2", URL: "http://head2.invalid:8000", Host: "same"},
		{Name: "worker2", URL: "http://worker2.invalid:8000", Host: "h3"},
	}
	r := replicaStaleRouter(t, cfgs, "head", "worker")
	decl2 := peers("head2", "head2", "worker2")
	for _, name := range []string{"head2", "worker2"} {
		n := nodeByName(t, r, name)
		n.mu.Lock()
		n.ReplicaPeers = decl2
		n.mu.Unlock()
	}
	setStaleByName(t, r, "worker", true)
	setStaleByName(t, r, "worker2", true)
	buf := captureLog(t)

	picked, _, _ := r.RouteExcluding("", "", map[string]bool{"http://head2.invalid:8000": true})
	if picked == nil || picked.Name != "head" {
		t.Fatalf("picked %v, want head", picked)
	}
	picked, _, _ = r.RouteExcluding("", "", map[string]bool{"http://head.invalid:8000": true})
	if picked == nil || picked.Name != "head2" {
		t.Fatalf("picked %v, want head2", picked)
	}
	if n := lastResortLogLines(buf); n != 2 {
		t.Errorf("two heads on one host logged %d lines, want 2 (one each):\n%s", n, buf.String())
	}
}

func TestLastResortNoLogWhenNothingSelected(t *testing.T) {
	r := headWithStaleWorker(t)
	buf := captureLog(t)
	picked, _, _ := r.RouteExcluding("", "", map[string]bool{"http://head.invalid:8000": true})
	if picked != nil {
		t.Fatalf("picked %v, want none", picked)
	}
	if n := lastResortLogLines(buf); n != 0 {
		t.Errorf("logged %d last-resort lines although nothing was routed", n)
	}
}

func TestLastResortWhenOnlyOtherNodeIsOverCapacity(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8000", Host: "h0"},
		{Name: "worker", URL: "http://worker.invalid:8000", Host: "h1"},
		{Name: "std", URL: "http://std.invalid:8000", Host: "h2"},
	}
	r := replicaStaleRouter(t, cfgs, "head", "worker")
	setStaleByName(t, r, "worker", true)
	std := nodeByName(t, r, "std")
	std.mu.Lock()
	std.MaxInFlight = 1
	std.mu.Unlock()
	atomic.StoreInt32(&std.ActiveConns, 1)

	picked, _, dec := r.Route("", "", "")
	if picked == nil || picked.Name != "head" || dec == nil {
		t.Fatalf("picked %v decision %+v, want head via last resort", picked, dec)
	}
	if !strings.Contains(dec.Detail, "last resort") || !strings.Contains(dec.Detail, "available or under capacity") {
		t.Errorf("detail %q should say no other node was available or under capacity", dec.Detail)
	}
	reasons := excludedReasons(dec.Excluded)
	if reasons["std"] != ExcludeReasonOverCapacity || reasons["head"] != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("excluded = %v, want std over_capacity and head replica_member_unreachable", reasons)
	}
}

func TestLastResortHeadListedPastExcludedCap(t *testing.T) {
	var cfgs []config.NodeConfig
	for i := 0; i < maxExcludedCandidates+5; i++ {
		cfgs = append(cfgs, config.NodeConfig{Name: fmt.Sprintf("n%02d", i), URL: fmt.Sprintf("http://n%02d.invalid:8000", i), Host: fmt.Sprintf("x%02d", i)})
	}
	cfgs = append(cfgs,
		config.NodeConfig{Name: "head", URL: "http://head.invalid:8000", Host: "h0"},
		config.NodeConfig{Name: "worker", URL: "http://worker.invalid:8000", Host: "h1"},
	)
	r := replicaStaleRouter(t, cfgs, "head", "worker")
	for _, n := range r.nodes {
		if strings.HasPrefix(n.Name, "n") {
			n.mu.Lock()
			n.Healthy = false
			n.mu.Unlock()
		}
	}
	setStaleByName(t, r, "worker", true)

	f := r.filterCandidatesWithFallback(r.nodes, "", "", nil)
	if len(f.lastResort) != 1 || f.lastResort[0].Name != "head" {
		t.Fatalf("lastResort = %v, want exactly head", f.lastResort)
	}
	picked, _, dec := r.Route("", "", "")
	if picked == nil || picked.Name != "head" || dec == nil {
		t.Fatalf("picked %v decision %+v, want head", picked, dec)
	}
	if got := excludedReasons(dec.Excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("chosen head missing from Excluded past the cap (reason %q)", got)
	}
	if dec.ExcludedTotal <= len(dec.Excluded) {
		t.Errorf("ExcludedTotal %d should exceed the %d listed entries when the cap truncated", dec.ExcludedTotal, len(dec.Excluded))
	}
}

func TestWithAffinityLostDetailWording(t *testing.T) {
	if got := withAffinityLostDetail("d", false, false); strings.Contains(got, "unhealthy") || strings.Contains(got, "draining") {
		t.Errorf("neutral lost-pin text must not name one cause: %q", got)
	}
	if got := withAffinityLostDetail("d", true, true); !strings.Contains(got, "skipped for this request and kept") {
		t.Errorf("last-resort pin text %q should say skipped for this request and kept", got)
	}
}

// TestExcludeReasonsListsEveryConstant keeps the exported list in step with the
// ExcludeReason constants declared in explain.go.
func TestExcludeReasonsListsEveryConstant(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "explain.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "ExcludeReason") {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				declared[v] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no ExcludeReason constants in explain.go")
	}
	listed := map[string]bool{}
	for _, v := range ExcludeReasons {
		listed[v] = true
	}
	for v := range declared {
		if !listed[v] {
			t.Errorf("ExcludeReasons is missing %q", v)
		}
	}
	if len(listed) != len(declared) || len(ExcludeReasons) != len(declared) {
		t.Errorf("ExcludeReasons has %d entries (%d distinct), explain.go declares %d", len(ExcludeReasons), len(listed), len(declared))
	}
}

// TestDashboardLabelsEveryExcludeReason fails when the Requests page has no
// display label for a reason the router can send.
func TestDashboardLabelsEveryExcludeReason(t *testing.T) {
	src, err := os.ReadFile("../../ui/src/pages/Requests.tsx")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "const EXCLUDED_REASON_LABELS")
	if start < 0 {
		t.Fatal("EXCLUDED_REASON_LABELS not found in Requests.tsx")
	}
	end := strings.Index(text[start:], "\n};")
	if end < 0 {
		t.Fatal("end of EXCLUDED_REASON_LABELS not found")
	}
	block := text[start : start+end]
	keyRe := regexp.MustCompile("(?m)^\\s*(\\w+):")
	keys := map[string]bool{}
	for _, m := range keyRe.FindAllStringSubmatch(block, -1) {
		keys[m[1]] = true
	}
	for _, reason := range ExcludeReasons {
		if !keys[reason] {
			t.Errorf("Requests.tsx EXCLUDED_REASON_LABELS has no key %q", reason)
		}
	}
}
