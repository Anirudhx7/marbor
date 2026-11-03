package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

func newNeverFitsRouter(t *testing.T) (*Router, *NodeState) {
	t.Helper()
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n1", URL: "http://n1.invalid", VRAMTotalMB: 1000},
	}, nil)
	n := r.nodes[0]
	n.VRAMTotalMB = 1000
	r.tagsCache[n.URL] = &TagsCache{
		Models: []TagModel{
			{Name: "huge", Size: 5000 * 1024 * 1024},
			{Name: "big", Size: 600 * 1024 * 1024},
		},
		FetchedAt: time.Now(),
	}
	return r, n
}

// A model larger than the whole node can never be loaded; the caller must be
// told to skip the load, not to proceed.
func TestMakeHeadroom_ModelLargerThanNodeReturnsNoHeadroom(t *testing.T) {
	r, n := newNeverFitsRouter(t)
	err := r.makeHeadroom(context.Background(), n, "huge")
	if !errors.Is(err, errNoHeadroom) {
		t.Fatalf("makeHeadroom = %v, want errNoHeadroom", err)
	}
}

// A model that fits alone but not alongside other in-flight warmups must also
// be skipped, and must give its own reservation back.
func TestMakeHeadroom_ExceedsWithOtherReservationsReturnsNoHeadroom(t *testing.T) {
	r, n := newNeverFitsRouter(t)
	r.reserveWarmBytes("n1", "other", 600*1024*1024)
	err := r.makeHeadroom(context.Background(), n, "big")
	if !errors.Is(err, errNoHeadroom) {
		t.Fatalf("makeHeadroom = %v, want errNoHeadroom", err)
	}
	if r.hasActiveWarmReservation("n1", "big") {
		t.Error("the skipped model's reservation must be cleared")
	}
}
