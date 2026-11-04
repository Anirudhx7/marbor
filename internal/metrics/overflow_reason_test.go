package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func reasonCount(t *testing.T, reason string) float64 {
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

func TestCloudOverflowReason_CountsPerReason(t *testing.T) {
	before := reasonCount(t, "test_reason")
	CloudOverflowReason("test_reason")
	CloudOverflowReason("test_reason")
	if got := reasonCount(t, "test_reason") - before; got != 2 {
		t.Errorf("counter delta = %v, want 2", got)
	}
}
