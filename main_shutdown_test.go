package main

// Proves, through the real main() wiring and a real SIGTERM, that the audit
// logger closes only after the HTTP servers drain, so the audit entry of every
// request still in flight at signal time reaches the store. main() has no
// separate shutdown function, so the test binary re-executes itself as the
// server (see TestMain).

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

const (
	runMainEnv    = "MARBOR_TEST_RUN_MAIN"
	runMainDBEnv  = "MARBOR_TEST_RUN_MAIN_DB"
	shutdownKey   = "sk-shutdown-test-key"
	inFlightCount = 8
	dropLogText   = "audit entries are being dropped"
)

// TestMain runs the server when the gate variable is set (only on the spawned
// child). A race in the child makes the race runtime override os.Exit(0).
func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		os.Args = []string{"marbor", "-db", os.Getenv(runMainDBEnv)}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// slowBackend is a fake ollama node. Until slow is set it answers chat
// immediately; afterwards it streams a chunk and holds the response open
// until release is closed.
type slowBackend struct {
	srv     *httptest.Server
	slow    atomic.Bool
	release chan struct{}
	once    sync.Once
}

func newSlowBackend(t *testing.T) *slowBackend {
	t.Helper()
	b := &slowBackend{release: make(chan struct{})}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3","model":"llama3"}]}`))
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/chat":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"model":"llama3","message":{"role":"assistant","content":"hi"},"done":false}` + "\n"))
			if b.slow.Load() {
				w.(http.Flusher).Flush()
				select {
				case <-b.release:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = w.Write([]byte(`{"model":"llama3","done":true,"eval_count":5,"prompt_eval_count":3}` + "\n"))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(func() {
		b.releaseAll()
		b.srv.Close()
	})
	return b
}

func (b *slowBackend) releaseAll() { b.once.Do(func() { close(b.release) }) }

// freePort reserves a port by binding ":0" (the proxy listens on all interfaces).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// seedShutdownDB writes the settings, node and key the server boots from.
func seedShutdownDB(t *testing.T, dbPath string, proxyPort, adminPort, metricsPort int, nodeURL string) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	settings := map[string]string{
		"proxy_port":           strconv.Itoa(proxyPort),
		"admin_bind_address":   fmt.Sprintf("127.0.0.1:%d", adminPort),
		"metrics_enabled":      "true",
		"metrics_bind_address": fmt.Sprintf("127.0.0.1:%d", metricsPort),
		"audit_enabled":        "true",
	}
	for k, v := range settings {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertNode(store.NodeRecord{Name: "fake-node", URL: nodeURL, Runtime: "ollama"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertKey(store.KeyRecord{Name: "shutdown-test", Key: shutdownKey}); err != nil {
		t.Fatal(err)
	}
}

// childEnv drops variables that would redirect the child database or backups
// and adds the gate variables; the parent environment is never modified.
func childEnv(dbPath string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MARBOR_DB_PATH=") || strings.HasPrefix(kv, "MARBOR_BACKUP_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, runMainEnv+"=1", runMainDBEnv+"="+dbPath)
}

type shutdownServer struct {
	cmd       *exec.Cmd
	logPath   string
	exited    chan error
	proxyAddr string
	adminAddr string
	metrics   string
	dbPath    string
}

// startShutdownServer boots the child, retrying on a port clash.
func startShutdownServer(t *testing.T, backend *slowBackend) *shutdownServer {
	t.Helper()
	for attempt := 0; attempt < 3; attempt++ {
		dbPath := t.TempDir() + string(os.PathSeparator) + "marbor.db"
		pp, ap, mp := freePort(t), freePort(t), freePort(t)
		seedShutdownDB(t, dbPath, pp, ap, mp, backend.srv.URL)

		s := &shutdownServer{
			logPath:   dbPath + ".log",
			exited:    make(chan error, 1),
			proxyAddr: fmt.Sprintf("127.0.0.1:%d", pp),
			adminAddr: fmt.Sprintf("127.0.0.1:%d", ap),
			metrics:   fmt.Sprintf("127.0.0.1:%d", mp),
			dbPath:    dbPath,
		}
		s.cmd = exec.Command(os.Args[0])
		s.cmd.Env = childEnv(dbPath)
		logFile, err := os.Create(s.logPath)
		if err != nil {
			t.Fatal(err)
		}
		defer logFile.Close()
		s.cmd.Stdout, s.cmd.Stderr = logFile, logFile
		if err := s.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { s.exited <- s.cmd.Wait() }()
		t.Cleanup(func() { _ = s.cmd.Process.Kill() })

		if s.waitListening(t) {
			return s
		}
		_ = s.cmd.Process.Kill()
	}
	t.Fatal("server never came up after 3 attempts")
	return nil
}

// log returns everything the child has written so far.
func (s *shutdownServer) log() string {
	b, _ := os.ReadFile(s.logPath)
	return string(b)
}

// waitListening waits until every listener has logged that it is serving and
// the admin health endpoint answers, which is past the signal registration.
// It returns false if the child died on a listen error.
func (s *shutdownServer) waitListening(t *testing.T) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out := s.log()
		for _, bad := range []string{"Proxy server error", "Admin server error", "Metrics server error"} {
			if strings.Contains(out, bad) {
				return false
			}
		}
		select {
		case err := <-s.exited:
			t.Fatalf("child exited during startup: %v\n%s", err, out)
		default:
		}
		if strings.Contains(out, "Proxy listening") && strings.Contains(out, "Admin dashboard listening") {
			if resp, err := http.Get("http://" + s.adminAddr + "/health"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return true
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server not ready within 30s\n%s", s.log())
	return false
}

func chatRequest(addr string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/chat",
		strings.NewReader(`{"model":"llama3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+shutdownKey)
	}
	return req, err
}

// warmUp retries chat until three succeed (the node must be polled first).
func (s *shutdownServer) warmUp(t *testing.T) []string {
	t.Helper()
	var ids []string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && len(ids) < 3 {
		req, _ := chatRequest(s.proxyAddr)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ids = append(ids, resp.Header.Get("X-Request-ID"))
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(ids) < 3 {
		t.Fatalf("no successful chat before deadline\n%s", s.log())
	}
	return ids
}

var droppedMetric = regexp.MustCompile(`(?m)^marbor_audit_dropped_total\s+(\S+)$`)

func (s *shutdownServer) assertNoDropsYet(t *testing.T, when string) {
	t.Helper()
	resp, err := http.Get("http://" + s.metrics + "/metrics")
	if err != nil {
		t.Fatalf("scrape metrics %s: %v", when, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := droppedMetric.FindSubmatch(body); m == nil || string(m[1]) != "0" {
		t.Fatalf("marbor_audit_dropped_total %s = %q, want 0", when, m)
	}
}

// startInFlight sends n requests the backend holds open and returns their IDs
// once all headers arrived (handlers mid-stream), plus a body-finishing func.
func (s *shutdownServer) startInFlight(t *testing.T, backend *slowBackend, n int) ([]string, func() error) {
	t.Helper()
	backend.slow.Store(true)
	results := make(chan *http.Response, n)
	for i := 0; i < n; i++ {
		go func() {
			req, _ := chatRequest(s.proxyAddr)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				resp = nil
			}
			results <- resp
		}()
	}
	var ids []string
	var resps []*http.Response
	for i := 0; i < n; i++ {
		select {
		case resp := <-results:
			if resp == nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("in-flight request %d failed: %v\n%s", i, resp, s.log())
			}
			ids = append(ids, resp.Header.Get("X-Request-ID"))
			resps = append(resps, resp)
		case <-time.After(20 * time.Second):
			t.Fatalf("only %d of %d in-flight responses started\n%s", i, n, s.log())
		}
	}
	return ids, func() error {
		for _, resp := range resps {
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || !strings.Contains(string(body), `"done":true`) {
				return fmt.Errorf("truncated in-flight body %q: %v", body, err)
			}
		}
		return nil
	}
}

// waitProxyRefusing blocks until the proxy listener closes (drain began).
func (s *shutdownServer) waitProxyRefusing(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", s.proxyAddr, 200*time.Millisecond)
		if err != nil {
			return
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("proxy still accepting 10s after SIGTERM\n%s", s.log())
}

func TestSignalShutdownFlushesInFlightAuditEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Process.Signal cannot deliver SIGTERM on Windows; verified on Linux CI")
	}
	backend := newSlowBackend(t)
	s := startShutdownServer(t, backend)

	warmIDs := s.warmUp(t)
	s.assertNoDropsYet(t, "before the signal")
	inFlightIDs, finish := s.startInFlight(t, backend, inFlightCount)

	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	s.waitProxyRefusing(t)
	// Sanity only: the real post-close detector is the drop log line below.
	s.assertNoDropsYet(t, "during the drain")

	backend.releaseAll()
	if err := finish(); err != nil {
		t.Fatalf("in-flight request did not complete: %v", err)
	}
	select {
	case err := <-s.exited:
		if err != nil {
			t.Fatalf("child exit: %v\n%s", err, s.log())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("child did not exit within 30s of release\n%s", s.log())
	}

	out := s.log()
	if !strings.Contains(out, "Shutdown complete") {
		t.Errorf("no clean shutdown line in output:\n%s", out)
	}
	for _, bad := range []string{"audit entries are being dropped", "AppendAuditLog failed"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}

	st, err := store.Open(s.dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	entries, err := st.QueryAuditLog(store.AuditQuery{Limit: 100000})
	if err != nil {
		t.Fatalf("QueryAuditLog: %v", err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[e.RequestID] = true
	}
	want := append(inFlightIDs, warmIDs...)
	for _, id := range want {
		if id == "" || !have[id] {
			t.Errorf("request %q has no audit entry", id)
		}
	}
	if len(entries) < len(want) {
		t.Errorf("audit rows = %d, want at least %d", len(entries), len(want))
	}
}
