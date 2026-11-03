package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// A panic inside a predictive prewarm goroutine must be recovered instead of
// taking the whole process down.
func TestRunTimeOfDayPrewarm_PanicInPrewarmIsRecovered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: srv.URL, VRAMTotalMB: 8192},
	}, nil)
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})
	for _, d := range []time.Time{
		time.Date(2026, 7, 2, 12, 30, 0, 0, time.Local),
		time.Date(2026, 7, 3, 12, 40, 0, 0, time.Local),
		time.Date(2026, 7, 4, 12, 20, 0, 0, time.Local),
	} {
		r.RecordTransition("model-tod", d)
	}
	r.tagsCache[srv.URL] = &TagsCache{
		Models:    []TagModel{{Name: "model-tod", Size: 1000 * 1024 * 1024}},
		FetchedAt: time.Now(),
	}
	r.nodes[0].VRAMTotalMB = 8192
	called := make(chan struct{}, 1)
	r.prewarmHook = func() {
		called <- struct{}{}
		panic("boom")
	}
	r.runTimeOfDayPrewarm(context.Background(), 12, []*NodeState{r.nodes[0]}, r.warmupCfg, "10m")
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("prewarm goroutine never ran")
	}
	time.Sleep(200 * time.Millisecond) // let the panic unwind
}
