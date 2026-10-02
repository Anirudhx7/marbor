package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// Adopt (overwrite a contradicting declaration with the detected group) and
// state-aware dismissal tests.

const adoptSnapshotWorker = `[{"node":"worker","head":"worker","members":["head","worker"]}]`

// makeContradiction declares a different head on the worker through the real
// node PATCH handler so the suggestion for the head/worker group turns
// contradicts_declared, and returns its fingerprint.
func (l *suggLab) makeContradiction() string {
	l.t.Helper()
	fp := l.onlyFingerprint()
	patch := httptest.NewRequest(http.MethodPatch, "/admin/nodes/worker", strings.NewReader(`{"replica_peers":{"members":["head","worker"],"head":"worker"}}`))
	patch.SetPathValue("name", "worker")
	rec := httptest.NewRecorder()
	l.s.handlePatchNode(rec, patch)
	if rec.Code != http.StatusOK {
		l.t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	return fp
}

// makeContradictionInMemory declares the contradicting value straight on the
// router, for a lab whose store refuses batch writes.
func (l *suggLab) makeContradictionInMemory() string {
	l.t.Helper()
	all := l.list("?includeDismissed=true")
	if len(all.Suggestions) != 1 {
		l.t.Fatalf("want exactly one suggestion, got %+v", all.Suggestions)
	}
	fp := all.Suggestions[0].Fingerprint
	if err := l.r.ApplyReplicaPeersBatch(map[string]store.ReplicaPeers{
		"worker": {Members: []string{"head", "worker"}, Head: "worker"},
	}, nil); err != nil {
		l.t.Fatal(err)
	}
	return fp
}

func (l *suggLab) confirm(fp, body string) *httptest.ResponseRecorder {
	l.t.Helper()
	return l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/confirm", body)
}

func (l *suggLab) dismissedRaw() []string { return l.s.loadDismissedSuggestions() }

func adoptBody(snapshot string) string {
	return `{"adopt":true,"declaredSnapshot":` + snapshot + `}`
}

func TestReplicaSuggestions_AdoptValidationAndRefusals(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		want     int
		wantText string
	}{
		{"missing snapshot is 400", `{"adopt":true}`, http.StatusBadRequest, "declaredSnapshot"},
		{"empty snapshot is 400", adoptBody(`[]`), http.StatusBadRequest, "declaredSnapshot"},
		{"duplicate node is 400", adoptBody(`[{"node":"worker","head":"worker","members":["head","worker"]},{"node":"worker","head":"worker","members":["head","worker"]}]`), http.StatusBadRequest, "duplicate"},
		{"malformed JSON is 400", `{"adopt":`, http.StatusBadRequest, ""},
		{"stale head is 409", adoptBody(`[{"node":"worker","head":"head","members":["head","worker"]}]`), http.StatusConflict, "changed"},
		{"stale members is 409", adoptBody(`[{"node":"worker","head":"worker","members":["worker"]}]`), http.StatusConflict, "changed"},
		{"extra node is 409", adoptBody(`[{"node":"worker","head":"worker","members":["head","worker"]},{"node":"head","head":"worker","members":["head","worker"]}]`), http.StatusConflict, "changed"},
		{"missing node is 409", adoptBody(`[{"node":"head","head":"worker","members":["head","worker"]}]`), http.StatusConflict, "changed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newSuggLab(t, nil)
			fp := l.makeContradiction()
			rec := l.confirm(fp, c.body)
			if rec.Code != c.want {
				t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), c.want)
			}
			if c.wantText != "" && !strings.Contains(rec.Body.String(), c.wantText) {
				t.Errorf("body lacks %q: %s", c.wantText, rec.Body.String())
			}
			if c.want == http.StatusConflict && !strings.Contains(rec.Body.String(), `"suggestion"`) {
				t.Errorf("a 409 must carry the fresh suggestion: %s", rec.Body.String())
			}
			if got := declaredOf(l.r, "head"); got != nil {
				t.Errorf("head must be untouched, got %+v", got)
			}
			if got := declaredOf(l.r, "worker"); got == nil || got.Head != "worker" {
				t.Errorf("worker declaration changed: %+v", got)
			}
		})
	}
}

func TestReplicaSuggestions_AdoptRefusedWhenAnOutsideNodeNamesAMember(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.makeContradiction()
	l.r.AddNode(config.NodeConfig{Name: "outsider", URL: "http://host-c:8000", Runtime: "vllm"})
	if err := l.r.ApplyReplicaPeersBatch(map[string]store.ReplicaPeers{
		"outsider": {Members: []string{"outsider", "worker"}, Head: "outsider"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	rec := l.confirm(fp, adoptBody(adoptSnapshotWorker))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outsider") {
		t.Errorf("the refusal must name the outside node: %s", rec.Body.String())
	}
	if declaredOf(l.r, "head") != nil {
		t.Error("head was written despite the refusal")
	}
	if got := declaredOf(l.r, "worker"); got == nil || got.Head != "worker" {
		t.Errorf("worker was overwritten despite the refusal: %+v", got)
	}
	if got := declaredOf(l.r, "outsider"); got == nil || got.Head != "outsider" {
		t.Errorf("outsider was changed: %+v", got)
	}
}

func TestReplicaSuggestions_AdoptOnCompleteSuggestionIs409(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	rec := l.confirm(fp, adoptBody(adoptSnapshotWorker))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Suggestion replicaSuggestionResp `json:"suggestion"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Suggestion.State != "complete" || !body.Suggestion.Confirmable {
		t.Errorf("fresh suggestion = %+v", body.Suggestion)
	}
	if declaredOf(l.r, "head") != nil || declaredOf(l.r, "worker") != nil {
		t.Error("adopt on a complete suggestion wrote something")
	}
}

func TestReplicaSuggestions_AdoptOnIncompleteSuggestionIs409(t *testing.T) {
	l := newSuggLab(t, nil)
	l.r.DropHostEvidence("host-b")
	got := l.list("")
	if len(got.Suggestions) != 1 || got.Suggestions[0].State != "incomplete" {
		t.Fatalf("suggestions = %+v", got.Suggestions)
	}
	if rec := l.confirm(got.Suggestions[0].Fingerprint, adoptBody(adoptSnapshotWorker)); rec.Code != http.StatusConflict {
		t.Errorf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestReplicaSuggestions_PlainConfirmOnContradictionStill409(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.makeContradiction()
	for _, body := range []string{"", "{}", `{"adopt":false}`, `{"adopt":false,"declaredSnapshot":` + adoptSnapshotWorker + `}`} {
		if rec := l.confirm(fp, body); rec.Code != http.StatusConflict {
			t.Errorf("body %q: status %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if declaredOf(l.r, "head") != nil {
		t.Error("a plain confirm wrote a declaration")
	}
}

func TestReplicaSuggestions_AdoptOverwritesEveryMemberAtomically(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.makeContradiction()
	// An unrelated pair with its own declaration is not touched.
	l.r.AddNode(config.NodeConfig{Name: "other-a", URL: "http://host-d:8000", Runtime: "vllm"})
	l.r.AddNode(config.NodeConfig{Name: "other-b", URL: "http://host-e:8000", Runtime: "vllm"})
	unrelated := store.ReplicaPeers{Members: []string{"other-a", "other-b"}, Head: "other-a"}
	if err := l.r.ApplyReplicaPeersBatch(map[string]store.ReplicaPeers{"other-a": unrelated, "other-b": unrelated}, nil); err != nil {
		t.Fatal(err)
	}

	// Unsorted and duplicated members in the snapshot are normalized.
	rec := l.confirm(fp, adoptBody(`[{"node":"worker","head":"worker","members":["worker","head","head"]}]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("adopt: %d %s", rec.Code, rec.Body.String())
	}
	var resp replicaSuggestionConfirmResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	roles := map[string]replicaSuggestionRoleResp{}
	for _, ro := range resp.Roles {
		roles[ro.Node] = ro
	}
	if roles["head"].Role != "head" || roles["worker"].Role != "worker" {
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
	for _, n := range []string{"other-a", "other-b"} {
		if got := declaredOf(l.r, n); got == nil || got.Head != "other-a" {
			t.Errorf("unrelated node %s changed: %+v", n, got)
		}
	}
	audit, _ := l.st.QuerySystemAuditLog(10)
	found := false
	for _, e := range audit {
		if e.Action == "confirm_replica_suggestion" && strings.Contains(e.Details, "Overwrote declarations on: worker") {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit row naming the overwritten node: %+v", audit)
	}
	if got := l.list(""); len(got.Suggestions) != 0 {
		t.Errorf("suggestions after adopt = %+v", got.Suggestions)
	}
}

func TestReplicaSuggestions_AdoptStoreFailureLeavesDeclarationsUnchanged(t *testing.T) {
	l := newSuggLab(t, func(st store.Store) store.Store { return failingBatchStore{st} })
	fp := l.makeContradictionInMemory()
	rec := l.confirm(fp, adoptBody(adoptSnapshotWorker))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if declaredOf(l.r, "head") != nil {
		t.Error("memory changed although the store write failed")
	}
	if got := declaredOf(l.r, "worker"); got == nil || got.Head != "worker" {
		t.Errorf("worker changed: %+v", got)
	}
}

func TestReplicaSuggestions_DismissalResurfacesWhenStateChanges(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	path := "/admin/replica-suggestions/" + fp + "/dismiss"

	if rec := l.do(http.MethodPost, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("dismiss: %d", rec.Code)
	}
	if raw := l.dismissedRaw(); len(raw) != 1 || raw[0] != fp+":complete" {
		t.Fatalf("stored = %v, want [%s:complete]", raw, fp)
	}
	if got := l.list(""); len(got.Suggestions) != 0 || got.DismissedCount != 1 {
		t.Fatalf("hidden list = %+v", got)
	}

	// The group turns into a conflict with a declaration: same fingerprint,
	// new state, so it must come back.
	l.makeContradictionInMemory()
	got := l.list("")
	if len(got.Suggestions) != 1 || got.Suggestions[0].State != "contradicts_declared" || got.Suggestions[0].Dismissed {
		t.Fatalf("after the state change = %+v", got)
	}
	if got.DismissedCount != 0 {
		t.Errorf("dismissedCount counts a suggestion that is visible again: %d", got.DismissedCount)
	}

	// Dismissing again records the new state and hides it again.
	if rec := l.do(http.MethodPost, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("second dismiss: %d", rec.Code)
	}
	if raw := l.dismissedRaw(); len(raw) != 1 || raw[0] != fp+":contradicts_declared" {
		t.Errorf("stored = %v, want a single contradicts_declared entry", raw)
	}
	if got := l.list(""); len(got.Suggestions) != 0 || got.DismissedCount != 1 {
		t.Errorf("hidden again = %+v", got)
	}
	got = l.list("?includeDismissed=true")
	if len(got.Suggestions) != 1 || !got.Suggestions[0].Dismissed {
		t.Errorf("includeDismissed = %+v", got)
	}
}

func TestReplicaSuggestions_DismissWithExplicitState(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	path := "/admin/replica-suggestions/" + fp + "/dismiss"

	// The client dismissed what it rendered: a state that is not current now.
	if rec := l.do(http.MethodPost, path, `{"state":"contradicts_declared"}`); rec.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body.String())
	}
	if raw := l.dismissedRaw(); len(raw) != 1 || raw[0] != fp+":contradicts_declared" {
		t.Fatalf("stored = %v", raw)
	}
	if got := l.list(""); len(got.Suggestions) != 1 || got.Suggestions[0].Dismissed {
		t.Errorf("the suggestion is complete now, a dismissal of another state must not hide it: %+v", got)
	}
	if rec := l.do(http.MethodPost, path, `{"state":"complete"}`); rec.Code != http.StatusOK {
		t.Fatalf("dismiss: %d", rec.Code)
	}
	if raw := l.dismissedRaw(); len(raw) != 1 || raw[0] != fp+":complete" {
		t.Errorf("a second dismissal must replace the first: %v", raw)
	}
	if got := l.list(""); len(got.Suggestions) != 0 {
		t.Errorf("not hidden: %+v", got)
	}
}

func TestReplicaSuggestions_DismissRejectsUnknownState(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	path := "/admin/replica-suggestions/" + fp + "/dismiss"
	for _, body := range []string{`{"state":"bogus"}`, `{"state":"complete:x"}`, `{"state":`} {
		if rec := l.do(http.MethodPost, path, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d", body, rec.Code)
		}
	}
	if raw := l.dismissedRaw(); len(raw) != 0 {
		t.Errorf("a rejected dismissal stored %v", raw)
	}
	for _, st := range []string{"complete", "incomplete", "conflicting", "contradicts_declared"} {
		if rec := l.do(http.MethodPost, path, `{"state":"`+st+`"}`); rec.Code != http.StatusOK {
			t.Errorf("state %s: status %d", st, rec.Code)
		}
	}
}

func TestReplicaSuggestions_LegacyPlainDismissalStillHidesAndRestoreRemovesBothForms(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	if err := store.SetJSONSetting(l.st, replicaSuggestionsDismissedKey, []string{fp}); err != nil {
		t.Fatal(err)
	}
	if got := l.list(""); len(got.Suggestions) != 0 || got.DismissedCount != 1 {
		t.Fatalf("legacy entry does not hide: %+v", got)
	}
	// It keeps hiding through a state change.
	l.makeContradictionInMemory()
	if got := l.list(""); len(got.Suggestions) != 0 {
		t.Errorf("legacy entry stopped hiding after a state change: %+v", got)
	}
	// Dismissing now replaces the legacy form with the stateful one.
	path := "/admin/replica-suggestions/" + fp + "/dismiss"
	if rec := l.do(http.MethodPost, path, ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if raw := l.dismissedRaw(); len(raw) != 1 || raw[0] != fp+":contradicts_declared" {
		t.Errorf("stored = %v", raw)
	}

	// Restore removes every form, including a hand-built mix.
	if err := store.SetJSONSetting(l.st, replicaSuggestionsDismissedKey, []string{fp, fp + ":complete", "0000000000000001:complete"}); err != nil {
		t.Fatal(err)
	}
	if rec := l.do(http.MethodDelete, path, ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if raw := l.dismissedRaw(); len(raw) != 1 || raw[0] != "0000000000000001:complete" {
		t.Errorf("restore left %v", raw)
	}
	if got := l.list(""); len(got.Suggestions) != 1 {
		t.Errorf("after restore = %+v", got)
	}
}

func TestReplicaSuggestions_DismissedCapCountsEachFingerprintOnce(t *testing.T) {
	l := newSuggLab(t, nil)
	first := fmt.Sprintf("%016x", 0)
	for i := 0; i < maxDismissedSuggestions; i++ {
		fp := fmt.Sprintf("%016x", i)
		if rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/dismiss", `{"state":"complete"}`); rec.Code != http.StatusOK {
			t.Fatalf("dismiss %d: %d", i, rec.Code)
		}
	}
	// Re-dismissing an existing fingerprint with other states must not push
	// the oldest one out.
	last := fmt.Sprintf("%016x", maxDismissedSuggestions-1)
	for _, st := range []string{"incomplete", "conflicting"} {
		if rec := l.do(http.MethodPost, "/admin/replica-suggestions/"+last+"/dismiss", `{"state":"`+st+`"}`); rec.Code != http.StatusOK {
			t.Fatal(rec.Code)
		}
	}
	raw := l.dismissedRaw()
	if len(raw) != maxDismissedSuggestions {
		t.Fatalf("len = %d, want %d", len(raw), maxDismissedSuggestions)
	}
	if !containsString(raw, first+":complete") {
		t.Error("the oldest fingerprint was evicted by a repeat dismissal")
	}
	if !containsString(raw, last+":conflicting") || containsString(raw, last+":complete") {
		t.Errorf("repeat dismissal did not replace the old entry: %v", raw[len(raw)-3:])
	}
}

func TestReplicaSuggestions_ConfirmStillWorksOnADismissedGroupWithState(t *testing.T) {
	l := newSuggLab(t, nil)
	fp := l.onlyFingerprint()
	l.do(http.MethodPost, "/admin/replica-suggestions/"+fp+"/dismiss", `{"state":"complete"}`)
	if rec := l.confirm(fp, ""); rec.Code != http.StatusOK {
		t.Errorf("confirm: %d %s", rec.Code, rec.Body.String())
	}
}
