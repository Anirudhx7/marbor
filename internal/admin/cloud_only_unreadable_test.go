package admin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// failingSetStore fails writes of one settings key.
type failingSetStore struct {
	store.Store
	failKey string
}

func (f *failingSetStore) SetSetting(key, value string) error {
	if key != "" && key == f.failKey {
		return errInjectedWrite
	}
	return f.Store.SetSetting(key, value)
}

type injectedErr string

func (e injectedErr) Error() string { return string(e) }

const errInjectedWrite = injectedErr("injected write failure")

func overflowSLAFromGET(t *testing.T, s *Server) int {
	t.Helper()
	m := getSettingsMap(t, s)
	var routing struct {
		OverflowSLAMs int `json:"overflow_sla_ms"`
	}
	if err := json.Unmarshal(m["routing"], &routing); err != nil {
		t.Fatal(err)
	}
	return routing.OverflowSLAMs
}

// A failed save of the list answers 500 and changes nothing: not the router,
// not the API view, not the stored row, not another setting in the same PUT.
func TestCloudOnly_PutPersistFailure_500AndNothingApplied(t *testing.T) {
	st := &failingSetStore{Store: openCloudOnlyStore(t)}
	s, r := queueSettingsServer(t, st)
	want := []string{"keep"}
	if rec := setCloudOnly(t, s, want); rec.Code >= 300 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	st.failKey = cloudOnlySettingKey
	body := putBody(t, s, map[string]any{"cloud_only_models": []string{"new"}, "overflow_sla_ms": 777})
	rec := doPut(s, s.AdminToken(), body)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with a failing save = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if got := r.CloudOnlyModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("router list = %v, want %v unchanged", got, want)
	}
	if got := cloudOnlyFromGET(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("GET list = %v, want %v unchanged", got, want)
	}
	if got := cloudOnlyFromDB(t, st.Store); !reflect.DeepEqual(got, want) {
		t.Errorf("stored list = %v, want %v unchanged", got, want)
	}
	if got := overflowSLAFromGET(t, s); got == 777 {
		t.Error("another setting from the failed PUT was applied")
	}
	// Positive control: the same body applies once the write works, so the
	// checks above are meaningful.
	st.failKey = ""
	if rec := doPut(s, s.AdminToken(), body); rec.Code >= 300 {
		t.Fatalf("PUT after the store recovers = %d: %s", rec.Code, rec.Body.String())
	}
	if got := r.CloudOnlyModels(); !reflect.DeepEqual(got, []string{"new"}) {
		t.Errorf("router list after a good save = %v, want [new]", got)
	}
	if got := overflowSLAFromGET(t, s); got != 777 {
		t.Errorf("overflow_sla_ms = %d after a good save, want 777", got)
	}
}

// unreadableServer starts a server whose router is in the unreadable state
// with a stored row that cannot be parsed, as after a boot that hit one.
func unreadableServer(t *testing.T) (*Server, *router.Router, store.Store) {
	t.Helper()
	st := openCloudOnlyStore(t)
	if err := st.SetSetting(cloudOnlySettingKey, `{not json`); err != nil {
		t.Fatal(err)
	}
	s, r := queueSettingsServer(t, st)
	r.SetCloudOnlyUnreadable()
	return s, r, st
}

// An unrelated settings save (the page echoes the empty list it can see) must
// not overwrite the unreadable row or lift the refusal state.
func TestCloudOnly_Unreadable_EmptyListPutRefused(t *testing.T) {
	s, r, st := unreadableServer(t)
	before := overflowSLAFromGET(t, s)
	rec := doPut(s, s.AdminToken(), putBody(t, s, map[string]any{"overflow_sla_ms": before + 300}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT with an empty list while unreadable = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !r.CloudOnlyUnreadable() {
		t.Error("the refusal state must stay while the list is unreadable")
	}
	if raw, _ := st.GetSetting(cloudOnlySettingKey); raw != `{not json` {
		t.Errorf("stored row = %q, want it untouched", raw)
	}
	if !strings.Contains(rec.Body.String(), "empty routing.cloud_only_models is refused") {
		t.Errorf("409 body = %s, want it to say an empty list is refused", rec.Body.String())
	}
	if got := overflowSLAFromGET(t, s); got != before {
		t.Errorf("overflow_sla_ms = %d after the refused PUT, want %d unchanged", got, before)
	}
	if got := cloudOnlyFromGET(t, s); len(got) != 0 {
		t.Errorf("GET list = %v, want the empty view the guard exists for", got)
	}
}

func TestCloudOnly_Unreadable_ExplicitListPutRecovers(t *testing.T) {
	s, r, st := unreadableServer(t)
	want := []string{"paid*"}
	if rec := setCloudOnly(t, s, want); rec.Code >= 300 {
		t.Fatalf("PUT with an explicit list = %d: %s", rec.Code, rec.Body.String())
	}
	if r.CloudOnlyUnreadable() || !r.IsCloudOnly("paid-1") || r.IsCloudOnly("other") {
		t.Error("an explicit list must replace the unreadable state")
	}
	if got := cloudOnlyFromDB(t, st); !reflect.DeepEqual(got, want) {
		t.Errorf("stored list = %v, want %v", got, want)
	}
}

// A failed save while unreadable must leave the router closed.
func TestCloudOnly_Unreadable_FailedSaveStaysClosed(t *testing.T) {
	st := &failingSetStore{Store: openCloudOnlyStore(t), failKey: cloudOnlySettingKey}
	s, r := queueSettingsServer(t, st)
	r.SetCloudOnlyUnreadable()
	if rec := setCloudOnly(t, s, []string{"x"}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT = %d, want 500", rec.Code)
	}
	if !r.CloudOnlyUnreadable() || !r.IsCloudOnly("anything") {
		t.Error("a failed save must not lift the refusal state")
	}
}

func TestCloudOnly_Unreadable_ReloadRecoversOnlyOnGoodRow(t *testing.T) {
	s, r, st := unreadableServer(t)
	if _, _, _, _, err := s.ReloadFromStore(); err == nil {
		t.Fatal("reload of a still-bad row must report an error")
	}
	if !r.CloudOnlyUnreadable() {
		t.Fatal("a failed reload must keep the refusal state")
	}
	if err := st.SetSetting(cloudOnlySettingKey, `["fixed"]`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatalf("reload of a repaired row: %v", err)
	}
	if r.CloudOnlyUnreadable() || !r.IsCloudOnly("fixed") || r.IsCloudOnly("other") {
		t.Error("a repaired row must replace the unreadable state")
	}
	s.mu.RLock()
	stale := s.cfg.Routing.CloudOnlyListUnreadable
	s.mu.RUnlock()
	if stale {
		t.Error("the config copy of the unreadable flag must be cleared by a good reload")
	}
}

// A store read error during reload keeps the refusal state too.
func TestCloudOnly_Unreadable_ReloadReadErrorKeepsClosed(t *testing.T) {
	inner := openCloudOnlyStore(t)
	st := &failingGetStore{Store: inner, failKey: cloudOnlySettingKey}
	s, r := queueSettingsServer(t, st)
	r.SetCloudOnlyUnreadable()
	if _, _, _, _, err := s.ReloadFromStore(); err == nil {
		t.Fatal("reload must report a failed read")
	}
	if !r.CloudOnlyUnreadable() {
		t.Error("a failed reload read must keep the refusal state")
	}
}
