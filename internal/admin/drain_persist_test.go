package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

const (
	drainPersistMsg   = "Drain is active on the node but could not be saved and will be lost on restart; retry the request"
	undrainPersistMsg = "Undrain is active on the node but could not be saved and the node will be drained again on restart; retry the request"
	rawPersistErr     = "disk exploded: secret-internal-detail"
)

// drainFailStore fails every SetNodeDrain call while failing is set, and
// counts the calls.
type drainFailStore struct {
	store.Store
	failing atomic.Bool
	calls   atomic.Int32
}

func (f *drainFailStore) SetNodeDrain(name string, draining bool, reason string, grace int) error {
	f.calls.Add(1)
	if f.failing.Load() {
		return errors.New(rawPersistErr)
	}
	return f.Store.SetNodeDrain(name, draining, reason, grace)
}

// drainPersistFixture is the sole-replica fixture with its store wrapped.
func drainPersistFixture(t *testing.T) (*Server, *drainFailStore) {
	t.Helper()
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	t.Cleanup(cleanup)
	fs := &drainFailStore{Store: s.st}
	s.SetStore(fs)
	readyNode(t, s, "a", "")
	readyNode(t, s, "b", "")
	return s, fs
}

func undrainQ(t *testing.T, s *Server, node string) *httptest.ResponseRecorder {
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
	if !strings.Contains(w.Body.String(), msg) {
		t.Errorf("body %q lacks %q", w.Body.String(), msg)
	}
	if strings.Contains(w.Body.String(), rawPersistErr) {
		t.Errorf("raw store error leaked: %s", w.Body.String())
	}
}

func TestDrainPersistFailure_Returns500AndAudits(t *testing.T) {
	s, fs := drainPersistFixture(t)
	fs.failing.Store(true)

	w := drainQ(t, s, "a", "")
	assertPersistFailure(t, w, drainPersistMsg)

	a := lastAudit(t, s)
	if a.Action != "drain_node" || !strings.HasSuffix(a.Details, "(not saved: persistence failed)") {
		t.Errorf("audit = %q / %q", a.Action, a.Details)
	}
}

func TestUndrainPersistFailure_Returns500AndAudits(t *testing.T) {
	s, fs := drainPersistFixture(t)
	if w := drainQ(t, s, "a", ""); w.Code != http.StatusOK {
		t.Fatalf("setup drain: %d %s", w.Code, w.Body.String())
	}
	fs.failing.Store(true)

	w := undrainQ(t, s, "a")
	assertPersistFailure(t, w, undrainPersistMsg)

	a := lastAudit(t, s)
	if a.Action != "undrain_node" || !strings.Contains(a.Details, "(not saved: persistence failed)") {
		t.Errorf("audit = %q / %q", a.Action, a.Details)
	}
}

func TestDrainUndrainPersistSuccess_StoresRow(t *testing.T) {
	s, fs := drainPersistFixture(t)

	if w := drainQ(t, s, "a", ""); w.Code != http.StatusOK {
		t.Fatalf("drain: %d %s", w.Code, w.Body.String())
	}
	states, err := fs.Store.NodeDrainStates()
	if err != nil || !states["a"].Draining {
		t.Fatalf("stored drain state = %+v, err %v", states["a"], err)
	}
	if a := lastAudit(t, s); strings.Contains(a.Details, "not saved") {
		t.Errorf("success audit marked unsaved: %q", a.Details)
	}

	if w := undrainQ(t, s, "a"); w.Code != http.StatusOK {
		t.Fatalf("undrain: %d %s", w.Code, w.Body.String())
	}
	states, err = fs.Store.NodeDrainStates()
	if err != nil || states["a"].Draining {
		t.Fatalf("stored state after undrain = %+v, err %v", states["a"], err)
	}
}

func TestDrainPersistFailure_ForcedSoleReplicaKeepsDetail(t *testing.T) {
	s, fs := drainPersistFixture(t)
	readyNode(t, s, "a", "", soleModel)
	fs.failing.Store(true)

	w := drainQ(t, s, "a", ackQuery(soleModel))
	assertPersistFailure(t, w, drainPersistMsg)
	d := lastAudit(t, s).Details
	if !strings.Contains(d, "Forced: true") || !strings.Contains(d, "(not saved: persistence failed)") {
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
	if n := fs.calls.Load(); n != 0 {
		t.Errorf("SetNodeDrain called %d times on a blocked drain", n)
	}
}

func TestDrainPersistFailure_LiveStateUnchanged(t *testing.T) {
	s, fs := drainPersistFixture(t)
	fs.failing.Store(true)

	assertPersistFailure(t, drainQ(t, s, "a", ""), drainPersistMsg)
	if !isDraining(s, "a") {
		t.Error("live drain was rolled back after a persistence failure")
	}

	assertPersistFailure(t, undrainQ(t, s, "a"), undrainPersistMsg)
	if isDraining(s, "a") {
		t.Error("live undrain was rolled back after a persistence failure")
	}
}

func TestDrainPersistFailure_RetryAfterRecovery(t *testing.T) {
	s, fs := drainPersistFixture(t)
	fs.failing.Store(true)
	assertPersistFailure(t, drainQ(t, s, "a", ""), drainPersistMsg)

	fs.failing.Store(false)
	// Retrying an already-live drain must succeed and save it, not 409/404.
	if w := drainQ(t, s, "a", ""); w.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	states, err := fs.Store.NodeDrainStates()
	if err != nil || !states["a"].Draining {
		t.Fatalf("stored drain state after retry = %+v, err %v", states["a"], err)
	}
}
