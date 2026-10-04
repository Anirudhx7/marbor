package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func adminIntp(v int) *int { return &v }

func suggVLLMDep(port, rank int, master string, headless bool) marboragent.DeploymentReport {
	t := &marboragent.Topology{
		Launcher: "vllm-mp", NNodes: adminIntp(2), NodeRank: adminIntp(rank),
		MasterAddr: master, MasterPort: adminIntp(29500), Evidence: []string{"cmdline:--nnodes"},
	}
	if headless {
		t.RoleHint = "worker"
	}
	return marboragent.DeploymentReport{Runtime: "vllm", Port: port, Topology: t}
}

func suggRPCDep(port int, servers ...string) marboragent.DeploymentReport {
	return marboragent.DeploymentReport{Runtime: "llamacpp", Port: port,
		Topology: &marboragent.Topology{Launcher: "llamacpp-rpc", RPCServers: servers}}
}

var suggCaps = []string{"status", "deployment.report", "deployment.topology"}

// suggLab is an admin server over a real router and a real on-disk store with
// a vLLM head on host-a and a headless worker on host-b already reporting.
type suggLab struct {
	t   *testing.T
	r   *router.Router
	st  store.Store
	s   *Server
	h   http.Handler
	tok string
}

func newSuggLab(t *testing.T, wrap func(store.Store) store.Store) *suggLab {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sugg.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	nodes := []config.NodeConfig{
		{Name: "head", URL: "http://host-a:8000", Runtime: "vllm"},
		{Name: "worker", URL: "http://host-b:8000", Runtime: "vllm"},
	}
	for _, n := range nodes {
		if err := st.UpsertNode(store.NodeRecord{Name: n.Name, URL: n.URL, Runtime: n.Runtime}); err != nil {
			t.Fatalf("UpsertNode: %v", err)
		}
	}
	r := router.New(config.RoutingConfig{}, nodes, nil)
	r.RecordHostEvidence(router.HostEvidence{Host: "host-a", Addrs: []string{"10.0.0.1"}, Capabilities: suggCaps,
		Deployments: []marboragent.DeploymentReport{suggVLLMDep(8000, 0, "10.0.0.1", false)}})
	r.RecordHostEvidence(router.HostEvidence{Host: "host-b", Addrs: []string{"10.0.0.2"}, Capabilities: suggCaps,
		Deployments: []marboragent.DeploymentReport{suggVLLMDep(0, 1, "10.0.0.1", true)}})
	use := st
	if wrap != nil {
		use = wrap(st)
	}
	s := NewServer(r, nil, config.Config{}, use)
	return &suggLab{t: t, r: r, st: st, s: s, h: s.Handler(), tok: s.AdminToken()}
}

func (l *suggLab) do(method, path, body string) *httptest.ResponseRecorder {
	l.t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: l.tok})
	rec := httptest.NewRecorder()
	l.h.ServeHTTP(rec, req)
	return rec
}

func (l *suggLab) list(query string) replicaSuggestionsListResp {
	l.t.Helper()
	rec := l.do(http.MethodGet, "/admin/replica-suggestions"+query, "")
	if rec.Code != http.StatusOK {
		l.t.Fatalf("list: status %d body %s", rec.Code, rec.Body.String())
	}
	var out replicaSuggestionsListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		l.t.Fatalf("decode: %v", err)
	}
	return out
}

func (l *suggLab) onlyFingerprint() string {
	l.t.Helper()
	got := l.list("")
	if len(got.Suggestions) != 1 {
		l.t.Fatalf("want exactly one suggestion, got %+v", got.Suggestions)
	}
	return got.Suggestions[0].Fingerprint
}

func declaredOf(r *router.Router, name string) *store.ReplicaPeers {
	for _, n := range r.Nodes() {
		if n.Name == name {
			n.RLock()
			defer n.RUnlock()
			if n.ReplicaPeers == nil {
				return nil
			}
			cp := *n.ReplicaPeers
			return &cp
		}
	}
	return nil
}

func TestReplicaSuggestions_ListShape(t *testing.T) {
	l := newSuggLab(t, nil)
	rec := l.do(http.MethodGet, "/admin/replica-suggestions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	raw := rec.Body.String()
	for _, want := range []string{`"suggestions":[`, `"coverage":[`, `"dismissedCount":0`, `"fingerprint":"`, `"confirmable":true`, `"dismissed":false`, `"missing":[]`, `"declared":[]`, `"state":"complete"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("response lacks %s: %s", want, raw)
		}
	}
	got := l.list("")
	s := got.Suggestions[0]
	if s.Head != "head" || len(s.Members) != 2 || s.Launcher != "vllm-mp" || s.Runtime != "vllm" || len(s.Evidence) != 2 {
		t.Errorf("suggestion = %+v", s)
	}
	if len(got.Coverage) != 2 || got.Coverage[0].State != "reporting" || !got.Coverage[0].Detected {
		t.Errorf("coverage = %+v", got.Coverage)
	}
	// /admin/v1 alias.
	if rec := l.do(http.MethodGet, "/admin/v1/replica-suggestions", ""); rec.Code != http.StatusOK {
		t.Errorf("v1 alias status %d", rec.Code)
	}
}

func TestReplicaSuggestions_Unauthorized(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/admin/replica-suggestions"},
		{http.MethodPost, "/admin/replica-suggestions/" + fp + "/confirm"},
		{http.MethodPost, "/admin/replica-suggestions/" + fp + "/dismiss"},
		{http.MethodDelete, "/admin/replica-suggestions/" + fp + "/dismiss"},
	} {
		rec := httptest.NewRecorder()
		l.h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session: %d, want 401", c.method, c.path, rec.Code)
		}
	}
	if declaredOf(l.r, "head") != nil {
		t.Error("an unauthorized confirm changed state")
	}
}

func TestReplicaSuggestions_ConfirmDeclaresEveryMemberAndFlipsRoles(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()

	rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	var resp replicaSuggestionConfirmResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	roles := map[string]replicaSuggestionRoleResp{}
	for _, ro := range resp.Roles {
		roles[ro.Node] = ro
	}
	if roles["head"].Role != "head" || roles["worker"].Role != "worker" || roles["worker"].Head != "head" {
		t.Errorf("roles = %+v", resp.Roles)
	}

	for _, n := range []string{"head", "worker"} {
		mem := declaredOf(l.r, n)
		if mem == nil || mem.Head != "head" || len(mem.Members) != 2 {
			t.Errorf("%s in memory = %+v", n, mem)
		}
	}
	ov, _ := l.st.NodeOverrides()
	for _, n := range []string{"head", "worker"} {
		if ov[n].ReplicaPeers == nil || ov[n].ReplicaPeers.Head != "head" {
			t.Errorf("%s in store = %+v", n, ov[n].ReplicaPeers)
		}
	}
	audit, _ := l.st.QuerySystemAuditLog(10)
	found := false
	for _, e := range audit {
		if e.Action == "confirm_replica_suggestion" && e.Target == "head" && strings.Contains(e.Details, "head,worker") {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit row for the confirm: %+v", audit)
	}
	// Already declared identically: nothing left to suggest.
	if got := l.list(""); len(got.Suggestions) != 0 {
		t.Errorf("suggestions after confirm = %+v", got.Suggestions)
	}
	// The node list agrees.
	nodesRec := l.do(http.MethodGet, "/admin/nodes", "")
	if !strings.Contains(nodesRec.Body.String(), `"schedulingRole":"worker"`) {
		t.Errorf("node list lacks the worker role: %s", nodesRec.Body.String())
	}
}

func TestReplicaSuggestions_ConfirmErrors(t *testing.T) {
	t.Run("unknown fingerprint is 404", func(t *testing.T) {
		l := newSuggLab(t, nil)
		if rec := l.do(http.MethodPost, "/admin/replica-suggestions/0123456789abcdef/confirm", ""); rec.Code != http.StatusNotFound {
			t.Errorf("status %d", rec.Code)
		}
	})
	t.Run("malformed fingerprint is 400", func(t *testing.T) {
		l := newSuggLab(t, nil)
		for _, fp := range []string{"nothex", "0123456789ABCDEF", "0123456789abcde"} {
			if rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", ""); rec.Code != http.StatusBadRequest {
				t.Errorf("fp %q: status %d", fp, rec.Code)
			}
		}
	})
	t.Run("incomplete suggestion is 400 and writes nothing", func(t *testing.T) {
		l := newSuggLab(t, nil)
		l.r.DropHostEvidence("host-b")
		got := l.list("")
		if len(got.Suggestions) != 1 || got.Suggestions[0].State != "incomplete" || got.Suggestions[0].Confirmable {
			t.Fatalf("suggestions = %+v", got.Suggestions)
		}
		rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+got.Suggestions[0].Fingerprint+"/confirm", "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d body %s", rec.Code, rec.Body.String())
		}
		if declaredOf(l.r, "head") != nil {
			t.Error("incomplete confirm wrote a declaration")
		}
	})
	t.Run("a contradicting declaration is 409 with the fresh suggestion", func(t *testing.T) {
		l := newSuggLab(t, nil)
		fp := l.onlyFingerprint()
		patch := httptest.NewRequest(http.MethodPatch, "/admin/nodes/worker", strings.NewReader(`{"replica_peers":{"members":["head","worker"],"head":"worker"}}`))
		patch.SetPathValue("name", "worker")
		rec := httptest.NewRecorder()
		l.s.handlePatchNode(rec, patch)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
		}
		rec = l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Error      string                `json:"error"`
			Suggestion replicaSuggestionResp `json:"suggestion"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Suggestion.State != "contradicts_declared" || body.Suggestion.Confirmable || len(body.Suggestion.Declared) != 1 {
			t.Errorf("fresh suggestion = %+v", body.Suggestion)
		}
		if got := declaredOf(l.r, "head"); got != nil {
			t.Errorf("head must be untouched, got %+v", got)
		}
	})
	t.Run("a fingerprint shared by two suggestions is 409", func(t *testing.T) {
		l := newSuggLab(t, nil)
		// Two rows on host-b; an unplaceable vLLM worker report and an
		// unplaceable llama.cpp report name the same rows with no head, so
		// the two conflicting suggestions share one fingerprint.
		l.r.AddNode(config.NodeConfig{Name: "worker-2", URL: "http://host-b:9000", Runtime: "auto"})
		l.r.RecordHostEvidence(router.HostEvidence{Host: "host-b", Addrs: []string{"10.0.0.2"}, Capabilities: suggCaps,
			Deployments: []marboragent.DeploymentReport{
				suggVLLMDep(0, 1, "10.0.0.1", true),
				suggRPCDep(0, "10.0.0.9:50052"),
			}})
		got := l.list("")
		byFP := map[string]int{}
		for _, sg := range got.Suggestions {
			byFP[sg.Fingerprint]++
		}
		shared := ""
		for fp, n := range byFP {
			if n > 1 {
				shared = fp
			}
		}
		if shared == "" {
			t.Fatalf("fixture did not produce a shared fingerprint: %+v", got.Suggestions)
		}
		if rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+shared+"/confirm", ""); rec.Code != http.StatusConflict {
			t.Errorf("status %d body %s", rec.Code, rec.Body.String())
		}
	})
}

// failingBatchStore fails the replica batch write with an error whose text
// must never reach a client.
type failingBatchStore struct{ store.Store }

func (failingBatchStore) SetReplicaPeersBatch(map[string]store.ReplicaPeers) error {
	return errors.New("secret-disk-path /var/lib/x exploded")
}

func TestReplicaSuggestions_StoreFailureLeavesMemoryAndStoreUnchanged(t *testing.T) {
	l := newSuggLab(t, func(st store.Store) store.Store { return failingBatchStore{st} })
	fp := l.onlyFingerprint()
	rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret-disk-path") || strings.Contains(rec.Body.String(), "exploded") {
		t.Errorf("raw store error reached the client: %s", rec.Body.String())
	}
	if declaredOf(l.r, "head") != nil || declaredOf(l.r, "worker") != nil {
		t.Error("memory changed although the store write failed")
	}
	ov, _ := l.st.NodeOverrides()
	if len(ov) != 0 {
		t.Errorf("store changed: %+v", ov)
	}
	audit, _ := l.st.QuerySystemAuditLog(10)
	for _, e := range audit {
		if e.Action == "confirm_replica_suggestion" {
			t.Error("a failed confirm must not be audited as applied")
		}
	}
}

func TestReplicaSuggestions_MemberMissingFromStoreWritesNothing(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	// The worker's row is gone from the store (as after a reload that removed
	// it) while the router still lists it.
	if err := l.st.DeleteNode("worker"); err != nil {
		t.Fatal(err)
	}
	rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if declaredOf(l.r, "head") != nil {
		t.Error("memory changed")
	}
	if ov, _ := l.st.NodeOverrides(); len(ov) != 0 {
		t.Errorf("a batch with a missing member wrote %+v", ov)
	}
}

func TestReplicaSuggestions_MemberRemovedFromRouterIs404Or409(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	if rec := l.do(http.MethodDelete, "/admin/nodes/worker", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d", rec.Code)
	}
	rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", "")
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusConflict {
		t.Errorf("status %d, want 404 or 409", rec.Code)
	}
	if ov, _ := l.st.NodeOverrides(); len(ov) != 0 {
		t.Errorf("an override row was written for a removed member: %+v", ov)
	}
}

// TestReplicaSuggestions_ConfirmVersusRemoveVersusReload races confirm against
// node removal and a config reload; whichever order wins, memory and store must
// agree and a failed confirm must leave nothing behind.
func TestReplicaSuggestions_ConfirmVersusRemoveVersusReload(t *testing.T) {
	for i := 0; i < 15; i++ {
		l := newSuggLab(t, nil)
		fp := l.onlyFingerprint()
		var confirmCode int
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			confirmCode = l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", "").Code
		}()
		go func() {
			defer wg.Done()
			l.do(http.MethodDelete, "/admin/nodes/worker", "")
		}()
		go func() {
			defer wg.Done()
			l.r.SyncNodes([]config.NodeConfig{{Name: "head", URL: "http://host-a:8000", Runtime: "vllm"}})
			_ = l.st.DeleteNode("worker")
		}()
		wg.Wait()

		ov, _ := l.st.NodeOverrides()
		if confirmCode == http.StatusOK {
			for _, n := range []string{"head", "worker"} {
				if ov[n].ReplicaPeers == nil {
					t.Fatalf("iteration %d: confirm succeeded but %s has no stored declaration", i, n)
				}
			}
			continue
		}
		if len(ov) != 0 || declaredOf(l.r, "head") != nil {
			t.Fatalf("iteration %d: confirm answered %d yet left state behind: store=%+v mem=%+v", i, confirmCode, ov, declaredOf(l.r, "head"))
		}
	}
}

func TestReplicaSuggestions_ConfirmSerializesWithPatch(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", "")
		}()
		go func(i int) {
			defer wg.Done()
			l.do(http.MethodPatch, "/admin/nodes/head", fmt.Sprintf(`{"max_in_flight":%d}`, i+1))
		}(i)
	}
	wg.Wait()
	if h := declaredOf(l.r, "head"); h == nil || h.Head != "head" {
		t.Errorf("head declaration = %+v", h)
	}
}

func TestReplicaSuggestions_DismissRestoreIdempotentAndPersistent(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	path := "/admin/replica-suggestions/" + fp + "/dismiss"

	for i := 0; i < 2; i++ {
		if rec := l.do(http.MethodPost, path, ""); rec.Code != http.StatusOK {
			t.Fatalf("dismiss #%d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if got := l.list(""); len(got.Suggestions) != 0 || got.DismissedCount != 1 {
		t.Errorf("hidden list = %+v", got)
	}
	got := l.list("?includeDismissed=true")
	if len(got.Suggestions) != 1 || !got.Suggestions[0].Dismissed || got.DismissedCount != 1 {
		t.Errorf("includeDismissed = %+v", got)
	}

	// A fresh server over the same store still sees the dismissal.
	s2 := NewServer(l.r, nil, config.Config{}, l.st)
	l2 := &suggLab{t: t, r: l.r, st: l.st, s: s2, h: s2.Handler(), tok: s2.AdminToken()}
	if got := l2.list(""); len(got.Suggestions) != 0 {
		t.Errorf("dismissal lost across reload: %+v", got)
	}

	for i := 0; i < 2; i++ {
		if rec := l.do(http.MethodDelete, path, ""); rec.Code != http.StatusOK {
			t.Fatalf("restore #%d: %d", i, rec.Code)
		}
	}
	if got := l.list(""); len(got.Suggestions) != 1 || got.DismissedCount != 0 {
		t.Errorf("after restore = %+v", got)
	}
}

func TestReplicaSuggestions_DismissNeverBlocksConfirm(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/dismiss", "")
	if rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", ""); rec.Code != http.StatusOK {
		t.Errorf("confirm of a dismissed suggestion: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReplicaSuggestions_DismissedListCappedAtOneHundred(t *testing.T) {
	l := newSuggLab(t, nil)
	for i := 0; i < maxDismissedSuggestions+5; i++ {
		fp := fmt.Sprintf("%016x", i)
		if rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/dismiss", ""); rec.Code != http.StatusOK {
			t.Fatalf("dismiss %d: %d", i, rec.Code)
		}
	}
	list, err := l.s.loadDismissedSuggestions()
	if err != nil {
		t.Fatalf("loadDismissedSuggestions: %v", err)
	}
	if len(list) != maxDismissedSuggestions {
		t.Fatalf("len = %d, want %d", len(list), maxDismissedSuggestions)
	}
	if containsString(list, fmt.Sprintf("%016x", 0)) || !containsString(list, fmt.Sprintf("%016x", maxDismissedSuggestions+4)) {
		t.Error("the oldest dismissals must be dropped first")
	}
}

func TestReplicaSuggestions_DismissRejectsMalformedFingerprint(t *testing.T) {
	l := newSuggLab(t, nil)
	if rec := l.do(http.MethodPost, "/admin/replica-suggestions/not-a-fingerprint/dismiss", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("status %d", rec.Code)
	}
}

func TestReplicaSuggestions_DismissalKeySurvivesSettingsWrites(t *testing.T) {
	// Nothing that enumerates settings may reject or drop the new key.
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/dismiss", "")
	all, err := l.st.AllSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all[replicaSuggestionsDismissedKey], fp) {
		t.Errorf("stored dismissals = %q", all[replicaSuggestionsDismissedKey])
	}
	if rec := l.do(http.MethodGet, "/admin/settings", ""); rec.Code != http.StatusOK {
		t.Errorf("settings endpoint after a dismissal: %d", rec.Code)
	}
}

// dismissedReadFailStore fails every read of the dismissed-suggestions
// setting while leaving all other settings readable.
type dismissedReadFailStore struct{ store.Store }

func (d dismissedReadFailStore) GetSetting(key string) (string, error) {
	if key == replicaSuggestionsDismissedKey {
		return "", errors.New("secret-disk-path /var/lib/x unreadable")
	}
	return d.Store.GetSetting(key)
}

const seededDismissals = `["aaaaaaaaaaaaaaaa:complete","bbbbbbbbbbbbbbbb"]`

// rawDismissed returns the stored value of the dismissed list and whether it
// exists, reading around any wrapper.
func (l *suggLab) rawDismissed() (string, bool) {
	l.t.Helper()
	v, err := l.st.GetSetting(replicaSuggestionsDismissedKey)
	if errors.Is(err, store.ErrNotFound) {
		return "", false
	}
	if err != nil {
		l.t.Fatalf("GetSetting: %v", err)
	}
	return v, true
}

func (l *suggLab) seedDismissed(raw string) {
	l.t.Helper()
	if err := l.st.SetSetting(replicaSuggestionsDismissedKey, raw); err != nil {
		l.t.Fatalf("SetSetting: %v", err)
	}
}

func TestReplicaSuggestions_DismissRestoreRefuseWhenListUnreadable(t *testing.T) {
	cases := []struct {
		name   string
		wrap   func(store.Store) store.Store
		stored string
	}{
		{"read error", func(st store.Store) store.Store { return dismissedReadFailStore{st} }, seededDismissals},
		{"corrupt json", nil, "{not json"},
		{"object instead of list", nil, `{"a":1}`},
		{"list of non-strings", nil, `[1,2]`},
		{"empty object", nil, `{}`},
		{"trailing garbage", nil, `[]x`},
		{"null entry", nil, `[null]`},
		{"null entry after a valid one", nil, `["a",null]`},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			for _, fpKind := range []string{"stored", "other"} {
				t.Run(tc.name+"/"+method+"/"+fpKind, func(t *testing.T) {
					l := newSuggLab(t, tc.wrap)
					l.seedDismissed(tc.stored)
					fp := "aaaaaaaaaaaaaaaa"
					if fpKind == "other" {
						fp = "cccccccccccccccc"
					}
					rec := l.do(method, "/admin/replica-suggestions/"+fp+"/dismiss", "")
					if rec.Code != http.StatusInternalServerError {
						t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
					}
					body := rec.Body.String()
					if !strings.Contains(body, replicaSuggestionsDismissedKey) || !strings.Contains(body, "nothing was changed") {
						t.Errorf("message must name the setting and say nothing changed: %s", body)
					}
					if strings.Contains(body, "secret-disk-path") {
						t.Errorf("raw store error reached the client: %s", body)
					}
					if got, _ := l.rawDismissed(); got != tc.stored {
						t.Errorf("stored value changed: %q -> %q", tc.stored, got)
					}
				})
			}
		}
	}
}

func TestReplicaSuggestions_ListFailsWhenDismissedListUnreadable(t *testing.T) {
	t.Run("read error", func(t *testing.T) {
		l := newSuggLab(t, func(st store.Store) store.Store { return dismissedReadFailStore{st} })
		l.seedDismissed(seededDismissals)
		rec := l.do(http.MethodGet, "/admin/replica-suggestions", "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), replicaSuggestionsDismissedKey) || strings.Contains(rec.Body.String(), "secret-disk-path") {
			t.Errorf("body %s", rec.Body.String())
		}
		if got, _ := l.rawDismissed(); got != seededDismissals {
			t.Errorf("a read must not rewrite the stored value, got %q", got)
		}
	})
	t.Run("corrupt json", func(t *testing.T) {
		l := newSuggLab(t, nil)
		l.seedDismissed("{not json")
		if rec := l.do(http.MethodGet, "/admin/replica-suggestions", ""); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		if got, _ := l.rawDismissed(); got != "{not json" {
			t.Errorf("a read must not rewrite the stored value, got %q", got)
		}
	})
}

func TestReplicaSuggestions_DismissedListReadableForms(t *testing.T) {
	cases := []struct {
		name     string
		stored   string // "" with set=false means the key is absent
		set      bool
		wantList int
	}{
		{"missing key", "", false, 0},
		{"empty string", "", true, 0},
		{"null", "null", true, 0},
		{"empty list", "[]", true, 0},
		{"whitespace only", "   ", true, 0},
		{"valid list", seededDismissals, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newSuggLab(t, nil)
			if tc.set {
				l.seedDismissed(tc.stored)
			}
			list, err := l.s.loadDismissedSuggestions()
			if err != nil || len(list) != tc.wantList {
				t.Fatalf("list %v err %v, want %d entries and no error", list, err, tc.wantList)
			}
			if rec := l.do(http.MethodGet, "/admin/replica-suggestions", ""); rec.Code != http.StatusOK {
				t.Errorf("list status %d", rec.Code)
			}
			rec := l.do(http.MethodPost, "/admin/replica-suggestions/cccccccccccccccc/dismiss", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("dismiss status %d body %s", rec.Code, rec.Body.String())
			}
			after, err := l.s.loadDismissedSuggestions()
			if err != nil || len(after) != tc.wantList+1 || !containsString(after, "cccccccccccccccc") {
				t.Errorf("after dismiss: %v err %v", after, err)
			}
			if tc.stored == seededDismissals {
				want := []string{"aaaaaaaaaaaaaaaa:complete", "bbbbbbbbbbbbbbbb"}
				if len(after) < len(want) || !reflect.DeepEqual(after[:len(want)], want) {
					t.Errorf("seeded entries did not survive the dismiss: %v", after)
				}
			}
		})
	}
}

func TestReplicaSuggestions_RestoreWithStoredNullWritesNothing(t *testing.T) {
	l := newSuggLab(t, nil)
	l.seedDismissed("null")
	if rec := l.do(http.MethodDelete, "/admin/replica-suggestions/aaaaaaaaaaaaaaaa/dismiss", ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if got, _ := l.rawDismissed(); got != "null" {
		t.Errorf("restore of a non-dismissed fingerprint rewrote the stored value: %q", got)
	}
}

func TestReplicaSuggestions_RestoreKeepsOtherDismissals(t *testing.T) {
	l := newSuggLab(t, nil)
	l.seedDismissed(seededDismissals)
	if rec := l.do(http.MethodDelete, "/admin/replica-suggestions/aaaaaaaaaaaaaaaa/dismiss", ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	list, err := l.s.loadDismissedSuggestions()
	if err != nil || len(list) != 1 || list[0] != "bbbbbbbbbbbbbbbb" {
		t.Errorf("list %v err %v, want only the other dismissal", list, err)
	}
}
