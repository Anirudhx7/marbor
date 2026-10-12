package router

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/store"
)

// fillHostile sets every settable field reachable from v to a hostile value:
// long strings with control and bidi characters, 200-entry lists, huge or
// negative numbers, NaN and infinite floats, and non-nil pointers.
func fillHostile(t *testing.T, v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("a" + string(rune(0x1b)) + "[31m" + string(rune(0x202e)) + "b\nc" + strings.Repeat("z", 999))
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(1 << 40)
	case reflect.Float64:
		v.SetFloat(math.Inf(1))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		fillHostile(t, v.Elem())
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 200, 200)
		for i := 0; i < s.Len(); i++ {
			fillHostile(t, s.Index(i))
		}
		v.Set(s)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				fillHostile(t, v.Field(i))
			}
		}
	default:
		t.Fatalf("fillHostile does not handle kind %s; extend it and checkBounded", v.Kind())
	}
}

// checkBounded fails for any string, list, integer or float in v that is outside
// what sanitizeTelemetry promises.
func checkBounded(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if len(s) > maxTelemetryStringBytes {
			t.Errorf("%s: string of %d bytes exceeds the cap", path, len(s))
		}
		for _, c := range s {
			if isUnsafeRune(c) {
				t.Errorf("%s: unsafe rune %U kept", path, c)
				break
			}
		}
	case reflect.Int, reflect.Int64, reflect.Int32:
		limit := maxTelemetryMB
		if strings.HasSuffix(path, "UptimeSeconds") || strings.HasSuffix(path, "BootTime") {
			limit = maxTelemetrySeconds
		}
		if n := v.Int(); n < 0 || n > limit {
			t.Errorf("%s: integer %d out of range", path, n)
		} else if v.Kind() == reflect.Int && n > maxTelemetryCount {
			t.Errorf("%s: counter %d above the counter cap", path, n)
		}
	case reflect.Float64:
		if f := v.Float(); math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > maxTelemetryDiskGB {
			t.Errorf("%s: float %v out of range", path, f)
		}
	case reflect.Ptr:
		if !v.IsNil() {
			checkBounded(t, path, v.Elem())
		}
	case reflect.Slice:
		if v.Len() > maxTelemetryListLen {
			t.Errorf("%s: list of %d exceeds the cap", path, v.Len())
		}
		for i := 0; i < v.Len(); i++ {
			checkBounded(t, path+"[]", v.Index(i))
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			checkBounded(t, path+"."+v.Type().Field(i).Name, v.Field(i))
		}
	case reflect.Bool:
	default:
		t.Errorf("%s: checkBounded does not handle kind %s; extend it", path, v.Kind())
	}
}

// TestSanitizeTelemetryBoundsEveryField fills the whole Telemetry type through
// reflection, so a field added later without a clamp fails here.
func TestSanitizeTelemetryBoundsEveryField(t *testing.T) {
	var tel marboragent.Telemetry
	fillHostile(t, reflect.ValueOf(&tel).Elem())
	sanitizeTelemetry(&tel)
	checkBounded(t, "Telemetry", reflect.ValueOf(tel))
}

func TestSanitizeTelemetryUsedNeverExceedsTotal(t *testing.T) {
	tel := marboragent.Telemetry{
		Host: &marboragent.HostTelemetry{RAMUsedMB: 900, RAMTotalMB: 100},
		GPU: &marboragent.GPUBlock{Devices: []marboragent.GPUInfo{
			{VRAMUsedMB: 500, VRAMTotalMB: 100},
			{VRAMUsedMB: 7, VRAMTotalMB: 0},
			{VRAMUsedMB: 3, VRAMTotalMB: 10},
		}},
	}
	sanitizeTelemetry(&tel)
	if tel.Host.RAMUsedMB != 100 {
		t.Errorf("RAMUsedMB = %d, want clamped to the total 100", tel.Host.RAMUsedMB)
	}
	noTotal := marboragent.Telemetry{Host: &marboragent.HostTelemetry{RAMUsedMB: 50}}
	sanitizeTelemetry(&noTotal)
	if noTotal.Host.RAMUsedMB != 50 {
		t.Errorf("RAMUsedMB = %d with no total, want the reading kept", noTotal.Host.RAMUsedMB)
	}
	want := []int64{100, 7, 3} // total 0 leaves the used reading as read
	for i, d := range tel.GPU.Devices {
		if d.VRAMUsedMB != want[i] {
			t.Errorf("device %d VRAMUsedMB = %d, want %d", i, d.VRAMUsedMB, want[i])
		}
	}
}

func TestClampIndicesAnyInvalidEntryMakesWholeListUnknown(t *testing.T) {
	if got := clampIndices([]int{0, 1, 2}); len(got) != 3 {
		t.Errorf("valid list changed: %v", got)
	}
	for _, in := range [][]int{{0, -1, 3}, {0, 5000}, make([]int, maxTelemetryListLen+1)} {
		if got := clampIndices(in); got != nil {
			t.Errorf("clampIndices(%v) = %v, want nil", in, got)
		}
	}
	if clampIndices(nil) != nil {
		t.Error("nil must stay nil")
	}
	tel := marboragent.Telemetry{Deployments: []marboragent.DeploymentReport{{
		GPUGroup: []int{0, -1, 3},
		GPUScope: &marboragent.GPUScope{Indices: []int{2, -9}},
	}}}
	sanitizeTelemetry(&tel)
	if d := tel.Deployments[0]; d.GPUGroup != nil || d.GPUScope.Indices != nil {
		t.Errorf("group %v / scope %v, want both unknown", d.GPUGroup, d.GPUScope.Indices)
	}
}

func TestPollAgentRejectsForeignJSON(t *testing.T) {
	for name, body := range map[string]string{
		"capabilities only": `{"capabilities":[]}`,
		"health only":       `{"health":{}}`,
		"node id only":      `{"agent":{"node_id":"x"}}`,
	} {
		body := body
		port := agentServerPort(t, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte(body)) })
		r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://127.0.0.1:11434"}}, nil)
		r.SetMarborAgent(r.nodes[0].Host, true, port, "tok", "http")
		r.pollAgentHosts()
		r.nodes[0].mu.RLock()
		present, failures := r.nodes[0].AgentPresent, r.nodes[0].AgentFailures
		r.nodes[0].mu.RUnlock()
		if present || failures != 1 {
			t.Errorf("%s: present=%v failures=%d, want an unhealthy agent", name, present, failures)
		}
	}
}

func TestLooksLikeAgentReportAcceptsEachOldSignal(t *testing.T) {
	stamp := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	for name, tel := range map[string]marboragent.Telemetry{
		"protocol version": {Agent: marboragent.Agent{ProtocolVersion: 1}},
		"platform":         {Agent: marboragent.Agent{Platform: "linux"}},
		"version":          {Agent: marboragent.Agent{Version: "v0.15.0"}},
		"last updated":     {LastUpdated: stamp},
	} {
		tel := tel
		if !looksLikeAgentReport(&tel) {
			t.Errorf("%s: rejected an agent report", name)
		}
	}
	if looksLikeAgentReport(&marboragent.Telemetry{}) {
		t.Error("an empty report must not look like an agent")
	}
}

// A flood of made-up model names (placeholder reservations) must not push out a
// real reservation, even when the real one is the oldest.
func TestWarmReservationCapEvictsPlaceholdersBeforeRealOnes(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	r.reserveWarmBytesAs("n", "real", 5*gibBytes, false)
	for i := 0; i < 3*maxWarmReservationsPerNode; i++ {
		r.reserveWarmBytesAs("n", fmt.Sprintf("made-up-%d", i), 1, true)
	}
	r.evictMu.Lock()
	res, ok := r.warmReserved["n"]["real"]
	size := len(r.warmReserved["n"])
	r.evictMu.Unlock()
	if !ok || res.bytes != 5*gibBytes {
		t.Fatalf("real reservation lost to a placeholder flood: %+v present=%v", res, ok)
	}
	if size > maxWarmReservationsPerNode {
		t.Fatalf("%d reservations held, want at most %d", size, maxWarmReservationsPerNode)
	}
}

// With only real reservations the oldest goes.
func TestWarmReservationCapFallsBackToOldestOverall(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	for i := 0; i < maxWarmReservationsPerNode; i++ {
		r.reserveWarmBytesAs("n", fmt.Sprintf("m-%d", i), 1, false)
		time.Sleep(time.Microsecond)
	}
	r.reserveWarmBytesAs("n", "newest", 1, false)
	r.evictMu.Lock()
	_, oldestKept := r.warmReserved["n"]["m-0"]
	_, newestKept := r.warmReserved["n"]["newest"]
	r.evictMu.Unlock()
	if oldestKept || !newestKept {
		t.Fatalf("oldest kept=%v newest kept=%v, want the oldest real reservation dropped", oldestKept, newestKept)
	}
}

// removingStore removes a node from the router while its warm rows are being
// read, which is the window RestoreWarmState cannot see through its snapshot.
type removingStore struct {
	store.Store
	onAll func()
}

func (s removingStore) AllWarmState() ([]store.WarmStateRecord, error) {
	rows, err := s.Store.AllWarmState()
	s.onAll()
	return rows, err
}

func TestRestoreWarmStateUndoesStampsOfNodeRemovedMidRestore(t *testing.T) {
	st := openWarmTestStore(t)
	used := time.Now().Add(-time.Hour).Round(time.Second)
	if err := st.RecordWarmLoad(store.WarmStateRecord{Model: "m", Node: "doomed", LastUsed: used, VRAMBytes: 1}); err != nil {
		t.Fatal(err)
	}
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "doomed", URL: "http://example.test:11434"}}, nil)
	r.SetStore(removingStore{Store: st, onAll: func() { r.RemoveNode("doomed") }})
	if _, err := r.RestoreWarmState(); err != nil {
		t.Fatal(err)
	}
	if !r.lastUsedAt("doomed", "m").IsZero() {
		t.Fatal("a node removed during the restore kept its restored last-used stamp")
	}
}

// A pinned skip must stay visible when the same run also had failures.
func TestFireScheduleKeepsPinnedWarningNextToFailures(t *testing.T) {
	rec := &unloadRecorder{status: http.StatusInternalServerError}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	r.SetPinnedModels("n", []string{"pinned"})
	r.schedules = []Schedule{{ID: "s1", Action: "unload", Node: "n", Models: []string{"m", "pinned"}}}
	r.fireSchedule(context.Background(), r.schedules[0])
	got := r.schedules[0]
	if got.LastStatus != "error" || !strings.Contains(got.LastError, "failed to unload") || !strings.Contains(got.LastError, "pinned model(s) skipped") {
		t.Fatalf("status %q error %q, want both the failure and the pinned warning", got.LastStatus, got.LastError)
	}
}

func TestFireScheduleLogHasNoTrailingSpaceWithoutWarning(t *testing.T) {
	logs := captureLog(t)
	rec := &unloadRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	r.schedules = []Schedule{{ID: "s1", Action: "unload", Node: "n", Models: []string{"m"}}}
	r.fireSchedule(context.Background(), r.schedules[0])
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "fired:") && strings.HasSuffix(line, " ") {
			t.Fatalf("log line ends with a space: %q", line)
		}
	}
}

// Model names in the scheduler status and audit note are quoted, so a name with
// a newline cannot add a line.
func TestFireScheduleQuotesModelNames(t *testing.T) {
	bad := "evil\nname"
	rec := &unloadRecorder{status: http.StatusInternalServerError}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	r.SetPinnedModels("n", []string{bad + "2"})
	r.schedules = []Schedule{{ID: "s1", Action: "unload", Node: "n", Models: []string{bad, bad + "2"}}}
	r.fireSchedule(context.Background(), r.schedules[0])
	if e := r.schedules[0].LastError; strings.ContainsAny(e, "\n\r") || !strings.Contains(e, `evil\nname`) {
		t.Fatalf("LastError = %q, want quoted names with no raw newline", e)
	}
}

// undoIfNodeRemoved takes the node lifecycle lock, the outermost router lock, so
// it must never be called while another router lock is held. This reads the
// package source and fails for any caller that has a lock taken by an enclosing
// or earlier statement of the same block still held at the call, or that
// defers an unlock. A lock taken and released inside a nested block, or inside
// a function literal, is not counted outside it.
func TestUndoIfNodeRemovedCallersHoldNoLock(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	callers := 0
	callName := func(s ast.Stmt) string {
		es, ok := s.(*ast.ExprStmt)
		if !ok {
			return ""
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok {
			return ""
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			return sel.Sel.Name
		}
		return ""
	}
	var check func(fn string, list []ast.Stmt, held int, deferred bool)
	check = func(fn string, list []ast.Stmt, held int, deferred bool) {
		for _, s := range list {
			switch callName(s) {
			case "Lock", "RLock":
				held++
				continue
			case "Unlock", "RUnlock":
				held--
				continue
			case "undoIfNodeRemoved":
				callers++
				if held > 0 || deferred {
					t.Errorf("%s calls undoIfNodeRemoved with a lock held (held=%d, deferred unlock=%v)", fn, held, deferred)
				}
				continue
			}
			if d, ok := s.(*ast.DeferStmt); ok {
				if sel, ok := d.Call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Unlock" || sel.Sel.Name == "RUnlock") {
					deferred = true
				}
				continue
			}
			h, d := held, deferred
			ast.Inspect(s, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncLit:
					return false
				case *ast.BlockStmt:
					check(fn, x.List, h, d)
					return false
				case *ast.CaseClause:
					check(fn, x.Body, h, d)
					return false
				case *ast.CommClause:
					check(fn, x.Body, h, d)
					return false
				}
				return true
			})
		}
	}
	idents := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "undoIfNodeRemoved" {
				idents++
			}
			return true
		})
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && fn.Name.Name != "undoIfNodeRemoved" {
				check(fn.Name.Name, fn.Body.List, 0, false)
			}
		}
	}
	// Every mention other than the declaration must be a call the walk saw, so a
	// call in an expression, or the function passed as a value, cannot slip by.
	if idents != callers+1 {
		t.Fatalf("%d mentions of undoIfNodeRemoved but the walk checked %d calls (plus the declaration); a call in an unchecked position", idents, callers)
	}
	if callers == 0 {
		t.Fatal("found no callers; the scan is not reading the package")
	}
}
