package router

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// Setting a list while the unreadable state is on never opens a window where a
// listed model reads as not listed.
func TestCloudOnly_UnreadableToListNoFailOpenWindow(t *testing.T) {
	for i := 0; i < 200; i++ {
		r := New(config.RoutingConfig{CloudOnlyListUnreadable: true}, nil, nil)
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.SetCloudOnlyModels([]string{"listed"})
		}()
		for finished := false; !finished; {
			if !r.IsCloudOnly("listed") {
				t.Fatal("a listed model read as not listed during the switch from unreadable to a list")
			}
			select {
			case <-done:
				finished = true
			default:
			}
		}
		if !r.IsCloudOnly("listed") || r.IsCloudOnly("other") {
			t.Fatal("the new list is not in force after the switch")
		}
	}
}
