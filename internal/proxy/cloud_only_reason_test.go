package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/audit"
	"github.com/prometheus/client_golang/prometheus"
)

func overflowReasonCount(t *testing.T, reason string) float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != "marbor_cloud_overflow_reason_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func auditReasonFor(t *testing.T, e *coEnv) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rows, err := e.h.audit.Query(audit.QueryOptions{Limit: 1}); err == nil && len(rows) > 0 {
			return rows[0].RoutingReason
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no audit row written")
	return ""
}

// Every refusal path records its own routing reason on the request-log row,
// the audit entry and the metric, while the API error code is unchanged. Only
// the unreadable-list refusal gets the distinct reason.
func TestProxy_CloudOnly_RefusalReasons(t *testing.T) {
	cases := []struct {
		name       string
		opts       coOpts
		unreadable bool
		path       string
		wantCode   string
		wantReason string
	}{
		{"unsupported path", coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}}, false, "/v1/rerank", "cloud_only_unsupported_path", CloudReasonCloudOnlyPolicy},
		{"no provider", coOpts{list: []string{"paid-model"}, noCloud: true, loaded: []string{"paid-model"}}, false, "/v1/chat/completions", "cloud_only_no_provider", CloudReasonCloudOnlyPolicy},
		{"budget exceeded", coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}, dailyCap: 1, withBudget: true}, false, "/v1/chat/completions", "cloud_budget_exceeded", CloudReasonCloudOnlyPolicy},
		{"list unreadable", coOpts{loaded: []string{"paid-model"}}, true, "/v1/chat/completions", "cloud_only_list_unreadable", CloudReasonCloudOnlyListUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCOEnv(t, tc.opts)
			e.h.audit = audit.New(e.st, true)
			if tc.unreadable {
				e.r.SetCloudOnlyUnreadable()
			}
			before := overflowReasonCount(t, tc.wantReason)
			rec := e.serve("POST", tc.path, `{"model":"paid-model","messages":[],"input":"x"}`)
			if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != tc.wantCode {
				t.Fatalf("status=%d body=%s, want 503 %s", rec.Code, rec.Body.String(), tc.wantCode)
			}
			if row := e.lastRow(t); row.RoutingReason != tc.wantReason {
				t.Errorf("row reason = %q, want %q", row.RoutingReason, tc.wantReason)
			}
			if got := auditReasonFor(t, e); got != tc.wantReason {
				t.Errorf("audit routing_reason = %q, want %q", got, tc.wantReason)
			}
			if got := overflowReasonCount(t, tc.wantReason) - before; got != 1 {
				t.Errorf("metric %q delta = %v, want 1", tc.wantReason, got)
			}
		})
	}
}

func TestCloudRoute_Sentence_ListUnreadable(t *testing.T) {
	c := &cloudRoute{reason: CloudReasonCloudOnlyListUnreadable}
	s := c.sentence()
	if !strings.Contains(s, "could not be read") || strings.Contains(s, "queue timeout") {
		t.Errorf("sentence = %q, want the unreadable-list sentence", s)
	}
}

func TestCloudReasonConstants_NoDuplicates(t *testing.T) {
	all := []string{
		CloudReasonNoNodeServesModel, CloudReasonAllNodesUnavailable, CloudReasonSaturatedSLAExpired,
		CloudReasonQueueTimeout, CloudReasonQueueFull, CloudReasonLocalUpstreamFailed,
		CloudReasonLocalOnlyBlocked, CloudReasonCloudOnlyPolicy, CloudReasonCloudOnlyListUnreadable,
	}
	seen := map[string]bool{}
	for _, r := range all {
		if seen[r] {
			t.Errorf("duplicate reason %q", r)
		}
		seen[r] = true
	}
}
