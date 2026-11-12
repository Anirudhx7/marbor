package admin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// unreadableFromGET reads routing.cloud_only_list_unreadable from GET
// /admin/settings; present reports whether the key is in the response at all.
func unreadableFromGET(t *testing.T, s *Server) (value, present bool) {
	t.Helper()
	m := getSettingsMap(t, s)
	var routing map[string]json.RawMessage
	if err := json.Unmarshal(m["routing"], &routing); err != nil {
		t.Fatal(err)
	}
	raw, ok := routing["cloud_only_list_unreadable"]
	if !ok {
		return false, false
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value, true
}

func cfgFlag(s *Server) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Routing.CloudOnlyListUnreadable
}

func TestSettings_CloudOnlyListUnreadableField_ShownOnlyWhileUnreadable(t *testing.T) {
	s, r, st := unreadableServer(t)
	if v, ok := unreadableFromGET(t, s); !ok || !v {
		t.Fatalf("GET while unreadable: present=%v value=%v, want present true", ok, v)
	}
	// A client sending the flag while the list is unreadable changes nothing:
	// the explicit list still recovers and the flag is gone afterwards.
	want := []string{"paid*"}
	if rec := doPut(s, s.AdminToken(), putBody(t, s, map[string]any{"cloud_only_models": want, "cloud_only_list_unreadable": true})); rec.Code >= 300 {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if r.CloudOnlyUnreadable() || cfgFlag(s) {
		t.Error("after a valid save the router and the config copy must both be clear")
	}
	if _, ok := unreadableFromGET(t, s); ok {
		t.Error("GET after a valid save must omit the flag")
	}
	if got := cloudOnlyFromDB(t, st); !reflect.DeepEqual(got, want) {
		t.Errorf("stored list = %v, want %v", got, want)
	}
}

// A client that sends true while the list is readable must not make the config
// or the router claim it is unreadable.
func TestSettings_CloudOnlyListUnreadableField_EchoedTrueIgnoredWhenReadable(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, r := queueSettingsServer(t, st)
	if rec := setCloudOnly(t, s, []string{"keep"}); rec.Code >= 300 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	rec := doPut(s, s.AdminToken(), putBody(t, s, map[string]any{"cloud_only_list_unreadable": true}))
	if rec.Code >= 300 {
		t.Fatalf("PUT echoing true = %d: %s", rec.Code, rec.Body.String())
	}
	if cfgFlag(s) || r.CloudOnlyUnreadable() {
		t.Error("an echoed true must not set the flag in the config or the router")
	}
	if _, ok := unreadableFromGET(t, s); ok {
		t.Error("GET must omit the flag while the list is readable")
	}
	if got := cloudOnlyFromGET(t, s); !reflect.DeepEqual(got, []string{"keep"}) {
		t.Errorf("list = %v, want it unchanged", got)
	}
}

// Echoing false while unreadable cannot lift the state (the 409 guard still
// reads the live router state), and the flag stays visible.
func TestSettings_CloudOnlyListUnreadableField_EchoedFalseCannotLiftState(t *testing.T) {
	s, r, _ := unreadableServer(t)
	rec := doPut(s, s.AdminToken(), putBody(t, s, map[string]any{"cloud_only_list_unreadable": false, "overflow_sla_ms": 400}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT = %d, want 409 for another setting saved while unreadable: %s", rec.Code, rec.Body.String())
	}
	if !r.CloudOnlyUnreadable() {
		t.Error("the router state must stay unreadable")
	}
	if v, ok := unreadableFromGET(t, s); !ok || !v {
		t.Errorf("GET: present=%v value=%v, want still shown", ok, v)
	}
}

func TestSettings_CloudOnlyListUnreadableField_ReloadBehaviour(t *testing.T) {
	s, r, st := unreadableServer(t)
	if _, _, _, _, err := s.ReloadFromStore(); err == nil {
		t.Fatal("reload of a bad row must report an error")
	}
	if v, ok := unreadableFromGET(t, s); !ok || !v {
		t.Errorf("after a failed reload GET: present=%v value=%v, want still shown", ok, v)
	}
	if err := st.SetSetting(cloudOnlySettingKey, `["fixed"]`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatal(err)
	}
	if _, ok := unreadableFromGET(t, s); ok || r.CloudOnlyUnreadable() {
		t.Error("a good reload must clear the flag everywhere")
	}
}
