package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

const (
	drainPersistMsg   = "Drain is active on the node but could not be saved and will be lost on restart; retry the request"
	undrainPersistMsg = "Undrain is active on the node but could not be saved and the node will be drained again on restart; retry the request"
	rawPersistErr     = "disk exploded: secret-internal-detail"
)

type setNodeDrainCall struct {
	name     string
	draining bool
	reason   string
	grace    int
}

// drainFailStore fails every SetNodeDrain call while failing is set, and
// records the calls.
type drainFailStore struct {
	store.Store
	failing atomic.Bool
	mu      sync.Mutex
	recs    []setNodeDrainCall
}

func (f *drainFailStore) SetNodeDrain(name string, draining bool, reason string, grace int) error {
	f.mu.Lock()
	f.recs = append(f.recs, setNodeDrainCall{name, draining, reason, grace})
	f.mu.Unlock()
	if f.failing.Load() {
		return errors.New(rawPersistErr)
	}
	return f.Store.SetNodeDrain(name, draining, reason, grace)
}

func (f *drainFailStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recs)
}

func (f *drainFailStore) lastCall(t *testing.T) setNodeDrainCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recs) == 0 {
		t.Fatal("SetNodeDrain was never called")
	}
	return f.recs[len(f.recs)-1]
}

// drainPersistFixture is the sole-replica fixture with its store wrapped.
func drainPersistFixture(t *testing.T) (*Server, *drainFailStore) {
	t.Helper()
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	t.Cleanup(cleanup)
	orig := s.st
	// The helper opens this store and does not close it; the fixture replaces
	// it below, so close it here (a store is closed once, only by this test).
	t.Cleanup(func() { _ = orig.Close() })
	fs := &drainFailStore{Store: orig}
	s.SetStore(fs)
	readyNode(t, s, "a", "")
	readyNode(t, s, "b", "")
	return s, fs
}

func undrainRequest(t *testing.T, s *Server, node string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/admin/nodes/"+node+"/drain", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func assertPersistFailure(t *testing.T, w *httptest.ResponseRecorder, msg string) {
	t.Helper()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, msg) {
		t.Errorf("body %q lacks %q", body, msg)
	}
	if !strings.Contains(body, "(request id: ") {
		t.Errorf("body %q lacks the request id suffix", body)
	}
	if strings.Contains(body, rawPersistErr) {
		t.Errorf("raw store error leaked: %s", body)
	}
}

func TestDrainUndrainPersistFailure_Returns500AndAudits(t *testing.T) {
	cases := []struct {
		name       string
		prepare    bool // drain first so the undrain has something to undo
		run        func(t *testing.T, s *Server) *httptest.ResponseRecorder
		msg        string
		action     string
		wantCall   setNodeDrainCall
		wantDetail func(d string) bool
	}{
		{
			name: "drain",
			run: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return drainQ(t, s, "a", "")
			},
			msg:      drainPersistMsg,
			action:   "drain_node",
			wantCall: setNodeDrainCall{"a", true, "manual", 0},
			wantDetail: func(d string) bool {
				return strings.HasSuffix(d, " "+notSavedAuditNote) && len(d) > len(notSavedAuditNote)+1
			},
		},
		{
			name:    "undrain",
			prepare: true,
			run: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return undrainRequest(t, s, "a")
			},
			msg:      undrainPersistMsg,
			action:   "undrain_node",
			wantCall: setNodeDrainCall{"a", false, "", 0},
			wantDetail: func(d string) bool {
				return d == notSavedAuditNote
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fs := drainPersistFixture(t)
			if tc.prepare {
				if w := drainQ(t, s, "a", ""); w.Code != http.StatusOK {
					t.Fatalf("setup drain: %d %s", w.Code, w.Body.String())
				}
			}
			fs.failing.Store(true)
			before := auditCount(t, s)

			assertPersistFailure(t, tc.run(t, s), tc.msg)

			if got := fs.lastCall(t); got != tc.wantCall {
				t.Errorf("SetNodeDrain args = %+v, want %+v", got, tc.wantCall)
			}
			if n := auditCount(t, s) - before; n != 1 {
				t.Errorf("%d audit rows written, want exactly 1", n)
			}
			a := lastAudit(t, s)
			if a.Action != tc.action || !tc.wantDetail(a.Details) {
				t.Errorf("audit = %q / %q", a.Action, a.Details)
			}
		})
	}
}

func TestDrainUndrainPersistSuccess_StoresRow(t *testing.T) {
	s, fs := drainPersistFixture(t)

	w := drainQ(t, s, "a", "")
	if w.Code != http.StatusOK {
		t.Fatalf("drain: %d %s", w.Code, w.Body.String())
	}
	if body := decodeBody(t, w); body["draining"] != true || body["reason"] != "manual" {
		t.Errorf("drain body = %s", w.Body.String())
	}
	if got, want := fs.lastCall(t), (setNodeDrainCall{"a", true, "manual", 0}); got != want {
		t.Errorf("SetNodeDrain args = %+v, want %+v", got, want)
	}
	states, err := fs.Store.NodeDrainStates()
	if err != nil || !states["a"].Draining {
		t.Fatalf("stored drain state = %+v, err %v", states["a"], err)
	}
	if a := lastAudit(t, s); strings.Contains(a.Details, notSavedAuditNote) {
		t.Errorf("success audit marked unsaved: %q", a.Details)
	}

	w = undrainRequest(t, s, "a")
	if w.Code != http.StatusOK {
		t.Fatalf("undrain: %d %s", w.Code, w.Body.String())
	}
	if body := decodeBody(t, w); body["draining"] != false {
		t.Errorf("undrain body = %s", w.Body.String())
	}
	if got, want := fs.lastCall(t), (setNodeDrainCall{"a", false, "", 0}); got != want {
		t.Errorf("SetNodeDrain args = %+v, want %+v", got, want)
	}
	states, err = fs.Store.NodeDrainStates()
	if err != nil || states["a"].Draining {
		t.Fatalf("stored state after undrain = %+v, err %v", states["a"], err)
	}
	if a := lastAudit(t, s); strings.Contains(a.Details, notSavedAuditNote) {
		t.Errorf("success audit marked unsaved: %q", a.Details)
	}
}

func TestDrainPersistFailure_ForcedSoleReplicaKeepsDetail(t *testing.T) {
	s, fs := drainPersistFixture(t)
	readyNode(t, s, "a", "", soleModel)
	fs.failing.Store(true)

	w := drainQ(t, s, "a", ackQuery(soleModel))
	assertPersistFailure(t, w, drainPersistMsg)
	d := lastAudit(t, s).Details
	if !strings.Contains(d, "Forced: true") || !strings.HasSuffix(d, " "+notSavedAuditNote) {
		t.Errorf("audit details %q", d)
	}
}

func TestDrain_BlockedSoleReplicaNeverTouchesStore(t *testing.T) {
	s, fs := drainPersistFixture(t)
	readyNode(t, s, "a", "", soleModel)
	fs.failing.Store(true)

	w := drainQ(t, s, "a", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
	if n := fs.callCount(); n != 0 {
		t.Errorf("SetNodeDrain called %d times on a blocked drain", n)
	}
	if isDraining(s, "a") {
		t.Error("a blocked drain left the node draining in the router")
	}
}

func TestDrainUndrain_UnknownNodeReturns404NeverTouchesStore(t *testing.T) {
	s, fs := drainPersistFixture(t)
	fs.failing.Store(true)

	if w := drainQ(t, s, "nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("drain status %d, want 404: %s", w.Code, w.Body.String())
	}
	if w := undrainRequest(t, s, "nope"); w.Code != http.StatusNotFound {
		t.Errorf("undrain status %d, want 404: %s", w.Code, w.Body.String())
	}
	if n := fs.callCount(); n != 0 {
		t.Errorf("SetNodeDrain called %d times for an unknown node", n)
	}
}

func TestDrainPersistFailure_LiveStateUnchanged(t *testing.T) {
	s, fs := drainPersistFixture(t)
	fs.failing.Store(true)

	assertPersistFailure(t, drainQ(t, s, "a", ""), drainPersistMsg)
	if !isDraining(s, "a") {
		t.Error("live drain was rolled back after a persistence failure")
	}
	if a := lastAudit(t, s); a.Action != "drain_node" || !strings.HasSuffix(a.Details, notSavedAuditNote) {
		t.Errorf("drain audit = %q / %q", a.Action, a.Details)
	}

	assertPersistFailure(t, undrainRequest(t, s, "a"), undrainPersistMsg)
	if isDraining(s, "a") {
		t.Error("live undrain was rolled back after a persistence failure")
	}
	if a := lastAudit(t, s); a.Action != "undrain_node" || a.Details != notSavedAuditNote {
		t.Errorf("undrain audit = %q / %q", a.Action, a.Details)
	}
}

func TestDrainPersistFailure_RetryAfterRecovery(t *testing.T) {
	s, fs := drainPersistFixture(t)
	fs.failing.Store(true)
	assertPersistFailure(t, drainQ(t, s, "a", ""), drainPersistMsg)
	if a := lastAudit(t, s); a.Action != "drain_node" || !strings.HasSuffix(a.Details, notSavedAuditNote) {
		t.Errorf("failed drain audit = %q / %q", a.Action, a.Details)
	}

	fs.failing.Store(false)
	// Retrying an already-live drain must succeed and save it, not 409/404.
	if w := drainQ(t, s, "a", ""); w.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	states, err := fs.Store.NodeDrainStates()
	if err != nil || !states["a"].Draining {
		t.Fatalf("stored drain state after retry = %+v, err %v", states["a"], err)
	}
	if a := lastAudit(t, s); a.Action != "drain_node" || strings.Contains(a.Details, notSavedAuditNote) {
		t.Errorf("retry audit = %q / %q", a.Action, a.Details)
	}
}
