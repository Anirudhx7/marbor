package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// Real-process check of the cloud overflow path: a request for a model that no
// local node can serve right now must reach the cloud provider in well under
// the queue timeout, and one served by a healthy node must stay local.
//
// The child is this test binary re-executed as marbor (see TestMain in
// main_shutdown_test.go), with a seeded database: one node, one cloud
// provider and one key.
func TestCloudOverflowHardIneligibleReachesCloudFast(t *testing.T) {
	var cloudHits, localHits atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[],"usage":{"total_tokens":10}}`))
	}))
	defer cloud.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3","model":"llama3"}]}`))
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[]}`))
		default:
			if strings.HasPrefix(r.URL.Path, "/v1/") {
				localHits.Add(1)
			}
			_, _ = w.Write([]byte(`{"id":"l","choices":[]}`))
		}
	}))
	defer local.Close()

	ports := freePorts(t, 3)
	dbPath := t.TempDir() + string(os.PathSeparator) + "marbor.db"
	st, err := store.Open(dbPath)
	must(t, err)
	for k, v := range map[string]string{
		"proxy_port":                       fmt.Sprint(ports[0]),
		"admin_bind_address":               fmt.Sprintf("127.0.0.1:%d", ports[1]),
		"metrics_enabled":                  "true",
		"metrics_bind_address":             fmt.Sprintf("127.0.0.1:%d", ports[2]),
		"routing_queue_timeout_ms":         "30000",
		"routing_overflow_sla_ms":          "2000",
		"routing_health_failure_threshold": "1",
	} {
		must(t, st.SetSetting(k, v))
	}
	must(t, st.UpsertNode(store.NodeRecord{Name: "fake-node", URL: local.URL, Runtime: "ollama"}))
	must(t, st.UpsertKey(store.KeyRecord{Name: "e2e", Key: shutdownKey}))
	must(t, st.UpsertCloudProvider(store.CloudProviderRecord{
		Name: "fake-openai", Provider: "openai", BaseURL: cloud.URL, APIKey: "k", Enabled: true,
	}))
	st.Close()

	logPath := dbPath + ".log"
	cmd := exec.Command(os.Args[0])
	cmd.Env = childEnv(dbPath)
	logFile, err := os.Create(logPath)
	must(t, err)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	must(t, cmd.Start())
	logFile.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	s := &shutdownServer{
		cmd: cmd, logPath: logPath, exited: make(chan error, 1),
		proxyAddr: fmt.Sprintf("127.0.0.1:%d", ports[0]),
		adminAddr: fmt.Sprintf("127.0.0.1:%d", ports[1]),
		metrics:   fmt.Sprintf("127.0.0.1:%d", ports[2]),
		dbPath:    dbPath,
	}
	go func() { s.exited <- cmd.Wait() }()
	if !s.waitListening(t) {
		t.Fatalf("server did not come up\n%s", s.log())
	}

	send := func() (int, time.Duration) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+s.proxyAddr+"/v1/chat/completions",
			bytes.NewReader([]byte(`{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+shutdownKey)
		start := time.Now()
		resp, err := probeClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v\n%s", err, s.log())
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, time.Since(start)
	}

	// Healthy node: served locally, never by cloud.
	deadline := time.Now().Add(20 * time.Second)
	for localHits.Load() == 0 && time.Now().Before(deadline) {
		send()
		time.Sleep(200 * time.Millisecond)
	}
	if localHits.Load() == 0 || cloudHits.Load() != 0 {
		t.Fatalf("healthy node: local=%d cloud=%d, want local only\n%s", localHits.Load(), cloudHits.Load(), s.log())
	}

	// Take the only node away. Once health marks it down, nothing local can
	// serve the model, so the request must go straight to cloud.
	local.Close()
	deadline = time.Now().Add(30 * time.Second)
	for cloudHits.Load() == 0 && time.Now().Before(deadline) {
		send()
		time.Sleep(300 * time.Millisecond)
	}
	if cloudHits.Load() == 0 {
		t.Fatalf("cloud never reached after the node went away\n%s", s.log())
	}
	// Until health marks the node down, a request still tries it, fails, and
	// retries on cloud. Wait for the first request that is routed without any
	// local attempt (no new upstream error in the log), then measure.
	var status int
	var took time.Duration
	var before int32
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		errs := strings.Count(s.log(), "upstream error")
		before = cloudHits.Load()
		status, took = send()
		if strings.Count(s.log(), "upstream error") == errs {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if status != http.StatusOK || cloudHits.Load() != before+1 {
		t.Fatalf("status=%d cloud hits %d->%d, want 200 and one more cloud hit\n%s", status, before, cloudHits.Load(), s.log())
	}
	t.Logf("all nodes down, cloud-bound request completed in %v (queue timeout 30s, overflow SLA 2s)", took)
	if took > time.Second {
		t.Errorf("hard-ineligible request took %v to reach cloud, want under 1s", took)
	}
}
