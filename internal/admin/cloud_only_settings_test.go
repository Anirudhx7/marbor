package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

const cloudOnlySettingKey = "routing_cloud_only_models"

func openCloudOnlyStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "co.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// cloudOnlyFromGET reads routing.cloud_only_models from GET /admin/settings.
func cloudOnlyFromGET(t *testing.T, s *Server) []string {
	t.Helper()
	m := getSettingsMap(t, s)
	var routing struct {
		CloudOnlyModels []string `json:"cloud_only_models"`
	}
	if err := json.Unmarshal(m["routing"], &routing); err != nil {
		t.Fatal(err)
	}
	return routing.CloudOnlyModels
}

// cloudOnlyFromDB reads the persisted list the way the boot loader does.
func cloudOnlyFromDB(t *testing.T, st store.Store) []string {
	t.Helper()
	var list []string
	store.GetJSONSetting(st, cloudOnlySettingKey, &list)
	return list
}

// putBody builds a full settings PUT body whose routing block carries routing.
// It must be called from the test goroutine (it reads settings with Fatal).
func putBody(t *testing.T, s *Server, routing map[string]any) []byte {
	t.Helper()
	m := getSettingsMap(t, s)
	var current map[string]any
	if err := json.Unmarshal(m["routing"], &current); err != nil {
		t.Fatal(err)
	}
	for k, v := range routing {
		current[k] = v
	}
	raw, _ := json.Marshal(current)
	m["routing"] = raw
	body, _ := json.Marshal(m)
	return body
}

func doPut(s *Server, token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func setCloudOnly(t *testing.T, s *Server, list []string) *httptest.ResponseRecorder {
	t.Helper()
	return doPut(s, s.AdminToken(), putBody(t, s, map[string]any{"cloud_only_models": list}))
}

func TestCloudOnly_SettingsPutRoundTrip(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, r := queueSettingsServer(t, st)

	if got := cloudOnlyFromGET(t, s); len(got) != 0 {
		t.Fatalf("default list = %v, want empty", got)
	}
	want := []string{"llama3", "qwen*"}
	if rec := setCloudOnly(t, s, want); rec.Code >= 300 {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if got := cloudOnlyFromGET(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("GET list = %v, want %v", got, want)
	}
	if got := r.CloudOnlyModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("router list = %v, want %v (live, no restart)", got, want)
	}
	if !r.IsCloudOnly("llama3:latest") || !r.IsCloudOnly("qwen2") || r.IsCloudOnly("mistral") {
		t.Error("router matching does not reflect the saved list")
	}
	if got := cloudOnlyFromDB(t, st); !reflect.DeepEqual(got, want) {
		t.Errorf("persisted list = %v, want %v", got, want)
	}

	// A PUT that omits the field leaves the list alone.
	if rec := doPut(s, s.AdminToken(), putBody(t, s, map[string]any{"overflow_sla_ms": 200})); rec.Code >= 300 {
		t.Fatalf("unrelated PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if got := cloudOnlyFromGET(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("list after an unrelated PUT = %v, want %v", got, want)
	}

	// An explicit empty list clears it everywhere.
	if rec := setCloudOnly(t, s, []string{}); rec.Code >= 300 {
		t.Fatalf("clearing PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if len(cloudOnlyFromGET(t, s)) != 0 || len(r.CloudOnlyModels()) != 0 || len(cloudOnlyFromDB(t, st)) != 0 {
		t.Error("an empty list must clear the API view, the router and the stored row")
	}
}

func TestCloudOnly_SettingsPutRejectsBadLists(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, r := queueSettingsServer(t, st)
	if rec := setCloudOnly(t, s, []string{"keep-me"}); rec.Code >= 300 {
		t.Fatalf("seed PUT = %d", rec.Code)
	}
	tooMany := make([]string, config.MaxCloudOnlyModels+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("m-%d", i)
	}
	cases := map[string][]string{
		"bare star":  {"*"},
		"mid star":   {"a*b"},
		"duplicate":  {"a", "A"},
		"whitespace": {"a b"},
		"empty":      {""},
		"too long":   {strings.Repeat("a", config.MaxModelAliasNameLen+1)},
		"too many":   tooMany,
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			rec := setCloudOnly(t, s, list)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if got := r.CloudOnlyModels(); !reflect.DeepEqual(got, []string{"keep-me"}) {
				t.Errorf("router list = %v after a rejected PUT, want it unchanged", got)
			}
			if got := cloudOnlyFromDB(t, st); !reflect.DeepEqual(got, []string{"keep-me"}) {
				t.Errorf("stored list = %v after a rejected PUT, want it unchanged", got)
			}
		})
	}
}

// The boot loader reads the stored row through the sanitizer: a stored entry
// that no longer validates is dropped, and the rest survive a restart.
func TestCloudOnly_PersistenceSurvivesRestartThroughSanitizer(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, _ := queueSettingsServer(t, st)
	want := []string{"llama3", "qwen*"}
	if rec := setCloudOnly(t, s, want); rec.Code >= 300 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	res := config.SanitizeCloudOnlyModels(cloudOnlyFromDB(t, st))
	got, dropped := res.Kept, res.Dropped
	if !reflect.DeepEqual(got, want) || len(dropped) != 0 {
		t.Fatalf("restart load = %v (dropped %v), want %v", got, dropped, want)
	}

	if err := st.SetSetting(cloudOnlySettingKey, `["ok","*","bad name","OK","fine*"]`); err != nil {
		t.Fatal(err)
	}
	res = config.SanitizeCloudOnlyModels(cloudOnlyFromDB(t, st))
	got, dropped = res.Kept, res.Dropped
	if !reflect.DeepEqual(got, []string{"ok", "fine*"}) || len(dropped) != 3 {
		t.Fatalf("restart load of a bad row = %v (dropped %v), want [ok fine*] and 3 dropped", got, dropped)
	}
}

func TestCloudOnly_ReloadPicksUpDBEdit(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, r := queueSettingsServer(t, st)
	if err := st.SetSetting(cloudOnlySettingKey, `["x","y*"]`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatalf("ReloadFromStore: %v", err)
	}
	want := []string{"x", "y*"}
	if got := cloudOnlyFromGET(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("GET list after reload = %v, want %v", got, want)
	}
	if got := r.CloudOnlyModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("router list after reload = %v, want %v", got, want)
	}
	// A later unrelated settings PUT must not undo the reload.
	if rec := doPut(s, s.AdminToken(), putBody(t, s, map[string]any{"overflow_sla_ms": 300})); rec.Code >= 300 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	if got := cloudOnlyFromGET(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("list after reload then an unrelated PUT = %v, want %v", got, want)
	}
	if got := r.CloudOnlyModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("router list after reload then an unrelated PUT = %v, want %v", got, want)
	}
}

func TestCloudOnly_ReloadClearedRowEmptiesList(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, r := queueSettingsServer(t, st)
	if rec := setCloudOnly(t, s, []string{"x"}); rec.Code >= 300 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	if err := st.SetSetting(cloudOnlySettingKey, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatalf("ReloadFromStore: %v", err)
	}
	if len(cloudOnlyFromGET(t, s)) != 0 || len(r.CloudOnlyModels()) != 0 {
		t.Errorf("a cleared row must give an empty list: GET %v router %v", cloudOnlyFromGET(t, s), r.CloudOnlyModels())
	}
}

// failingGetStore fails reads of one settings key.
type failingGetStore struct {
	store.Store
	failKey string
}

func (f *failingGetStore) GetSetting(key string) (string, error) {
	if key == f.failKey {
		return "", fmt.Errorf("injected read failure")
	}
	return f.Store.GetSetting(key)
}

func TestCloudOnly_ReloadReadErrorKeepsList(t *testing.T) {
	inner := openCloudOnlyStore(t)
	st := &failingGetStore{Store: inner}
	s, r := queueSettingsServer(t, st)
	want := []string{"keep"}
	if rec := setCloudOnly(t, s, want); rec.Code >= 300 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	st.failKey = cloudOnlySettingKey
	if _, _, _, _, err := s.ReloadFromStore(); err == nil {
		t.Fatal("ReloadFromStore must report a failed cloud-only read")
	}
	if got := cloudOnlyFromGET(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("GET list after a failed read = %v, want %v kept", got, want)
	}
	if got := r.CloudOnlyModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("router list after a failed read = %v, want %v kept", got, want)
	}
}

func TestCloudOnly_ReloadInvalidJSONKeepsList(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, r := queueSettingsServer(t, st)
	want := []string{"keep"}
	if rec := setCloudOnly(t, s, want); rec.Code >= 300 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	if err := st.SetSetting(cloudOnlySettingKey, `{not json`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.ReloadFromStore(); err == nil {
		t.Fatal("ReloadFromStore must report an unparseable cloud-only row")
	}
	if got := cloudOnlyFromGET(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("GET list after invalid JSON = %v, want %v kept", got, want)
	}
	if got := r.CloudOnlyModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("router list after invalid JSON = %v, want %v kept", got, want)
	}
}

// consistent asserts the API view, the router and the stored row agree.
func consistent(t *testing.T, s *Server, r *router.Router, st store.Store) []string {
	t.Helper()
	api := cloudOnlyFromGET(t, s)
	live := r.CloudOnlyModels()
	db := cloudOnlyFromDB(t, st)
	if !reflect.DeepEqual(api, live) || !reflect.DeepEqual(api, db) {
		t.Fatalf("lists disagree: GET %v, router %v, stored %v", api, live, db)
	}
	return api
}

// A reload racing settings PUTs must end with s.cfg (what GET serves), the
// router and the stored row all agreeing. Run under -race.
func TestReload_RacesSettingsPUT_CloudOnly(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, r := queueSettingsServer(t, st)
	listA, listB := []string{"alpha", "a*"}, []string{"beta"}
	bodyA := putBody(t, s, map[string]any{"cloud_only_models": listA})
	bodyB := putBody(t, s, map[string]any{"cloud_only_models": listB})
	token := s.AdminToken()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				body := bodyA
				if (i+w)%2 == 1 {
					body = bodyB
				}
				if rec := doPut(s, token, body); rec.Code >= 300 {
					t.Errorf("PUT = %d: %s", rec.Code, rec.Body.String())
					return
				}
			}
		}(w)
	}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, _, _, _, err := s.ReloadFromStore(); err != nil {
					t.Errorf("ReloadFromStore: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got := consistent(t, s, r, st)
	if !reflect.DeepEqual(got, listA) && !reflect.DeepEqual(got, listB) {
		t.Fatalf("final list = %v, want one of the submitted lists", got)
	}
}

// Two concurrent PUTs serialize: the live list equals the last accepted one.
func TestCloudOnly_ConcurrentPuts_LastWriterWins(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, r := queueSettingsServer(t, st)
	listA, listB := []string{"one"}, []string{"two", "three*"}
	bodyA := putBody(t, s, map[string]any{"cloud_only_models": listA})
	bodyB := putBody(t, s, map[string]any{"cloud_only_models": listB})
	token := s.AdminToken()

	var wg sync.WaitGroup
	for _, body := range [][]byte{bodyA, bodyB} {
		wg.Add(1)
		go func(b []byte) {
			defer wg.Done()
			if rec := doPut(s, token, b); rec.Code >= 300 {
				t.Errorf("PUT = %d: %s", rec.Code, rec.Body.String())
			}
		}(body)
	}
	wg.Wait()

	got := consistent(t, s, r, st)
	if !reflect.DeepEqual(got, listA) && !reflect.DeepEqual(got, listB) {
		t.Fatalf("final list = %v, want exactly one of the two submitted lists", got)
	}
}

// The reload takes the settings lock for its cloud-only step: while a settings
// PUT holds it (between its live update and its persist), the reload must wait
// rather than read the older stored list and put it back on the router.
func TestReload_WaitsForSettingsPUT_CloudOnly(t *testing.T) {
	st := openCloudOnlyStore(t)
	s, _ := queueSettingsServer(t, st)

	s.settingsMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, _, _, err := s.ReloadFromStore(); err != nil {
			t.Errorf("ReloadFromStore: %v", err)
		}
	}()
	select {
	case <-done:
		s.settingsMu.Unlock()
		t.Fatal("ReloadFromStore finished while a settings PUT held the settings lock")
	case <-time.After(200 * time.Millisecond):
	}
	s.settingsMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReloadFromStore did not finish after the settings lock was released")
	}
}
