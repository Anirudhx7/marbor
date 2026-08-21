package marboragent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/marboragent/control"
)

// fakeControlDriver satisfies control.ControlDriver for tests, letting a
// test observe which verb was called and control the returned error without
// depending on any real subprocess execution.
type fakeControlDriver struct {
	startErr, stopErr, restartErr error
	logsOut                       []string
	logsErr                       error
	called                        []string
}

func (f *fakeControlDriver) Name() string       { return "fake" }
func (f *fakeControlDriver) Requires() []string { return nil }
func (f *fakeControlDriver) Start(ctx context.Context) error {
	f.called = append(f.called, "start")
	return f.startErr
}
func (f *fakeControlDriver) Stop(ctx context.Context) error {
	f.called = append(f.called, "stop")
	return f.stopErr
}
func (f *fakeControlDriver) Restart(ctx context.Context) error {
	f.called = append(f.called, "restart")
	return f.restartErr
}
func (f *fakeControlDriver) Status(ctx context.Context) (control.Status, error) {
	return control.Status{}, nil
}
func (f *fakeControlDriver) Logs(ctx context.Context, lines int) ([]string, error) {
	f.called = append(f.called, "logs")
	return f.logsOut, f.logsErr
}
func (f *fakeControlDriver) Validate(ctx context.Context) error { return nil }

func withFakeControlDriver(t *testing.T, fake *fakeControlDriver) {
	t.Helper()
	old := newControlDriver
	newControlDriver = func(driver, identifier, startCommand string) (control.ControlDriver, error) {
		return fake, nil
	}
	t.Cleanup(func() { newControlDriver = old })
}

func doRuntimeAction(t *testing.T, s *Server, action string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/runtime/"+action, strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func newControlActionTestServer() *Server {
	return &Server{Token: "test-token"}
}

func doRuntimeLogs(t *testing.T, s *Server, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/runtime/logs", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// TestHandleRuntimeLogs_ConfiguredSuccess verifies the configured success
// path: the request's driver/identifier builds a ControlDriver and Logs is
// invoked, returning its lines verbatim.
func TestHandleRuntimeLogs_ConfiguredSuccess(t *testing.T) {
	fake := &fakeControlDriver{logsOut: []string{"line one", "line two"}}
	withFakeControlDriver(t, fake)
	s := newControlActionTestServer()

	w := doRuntimeLogs(t, s, map[string]interface{}{"driver": "systemd", "identifier": "ollama.service", "lines": 50})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp logsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Lines) != 2 || resp.Lines[0] != "line one" || resp.Lines[1] != "line two" {
		t.Fatalf("Lines = %v, want [line one line two]", resp.Lines)
	}
	if len(fake.called) != 1 || fake.called[0] != "logs" {
		t.Fatalf("expected logs called once, got %v", fake.called)
	}
}

// TestHandleRuntimeLogs_Unconfigured verifies the same mandated error as
// start/stop/restart when the mesh sends no driver.
func TestHandleRuntimeLogs_Unconfigured(t *testing.T) {
	s := newControlActionTestServer()

	w := doRuntimeLogs(t, s, map[string]interface{}{})

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", w.Code, w.Body.String())
	}
	var resp logsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != "Runtime control unavailable: no control driver configured" {
		t.Fatalf("error = %q, want the exact mandated message", resp.Error)
	}
}

// TestHandleRuntimeLogs_DriverExecutionError verifies a real driver failure
// (e.g. journalctl failing) is relayed as a 502 with the driver's own error
// text, never swallowed into an empty-looking success.
func TestHandleRuntimeLogs_DriverExecutionError(t *testing.T) {
	fake := &fakeControlDriver{logsErr: errors.New("journalctl: unit ollama.service not found")}
	withFakeControlDriver(t, fake)
	s := newControlActionTestServer()

	w := doRuntimeLogs(t, s, map[string]interface{}{"driver": "systemd", "identifier": "ollama.service"})

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", w.Code, w.Body.String())
	}
	var resp logsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Fatalf("error = %q, want it to contain the driver's real error text", resp.Error)
	}
}

// TestHandleRuntimeAction_ConfiguredSuccess verifies the configured success
// path for all three verbs: the request's driver/identifier builds a
// ControlDriver and the matching method is invoked.
func TestHandleRuntimeAction_ConfiguredSuccess(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			fake := &fakeControlDriver{}
			withFakeControlDriver(t, fake)
			s := newControlActionTestServer()

			w := doRuntimeAction(t, s, action, map[string]string{"driver": "systemd", "identifier": "ollama.service"})

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
			}
			var resp actionResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !resp.OK {
				t.Fatalf("expected ok=true, got %+v", resp)
			}
			if len(fake.called) != 1 || fake.called[0] != action {
				t.Fatalf("expected %q called once, got %v", action, fake.called)
			}
		})
	}
}

// TestHandleRuntimeAction_Unconfigured verifies the exact error
// node-agent-capabilities.md section 5.6 mandates when the mesh sends no
// driver (nothing configured for this node) - never a guess.
func TestHandleRuntimeAction_Unconfigured(t *testing.T) {
	s := newControlActionTestServer()

	w := doRuntimeAction(t, s, "restart", map[string]string{})

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", w.Code, w.Body.String())
	}
	var resp actionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != "Runtime control unavailable: no control driver configured" {
		t.Fatalf("error = %q, want the exact mandated message", resp.Error)
	}
}

// TestHandleRuntimeAction_DriverExecutionError verifies a real driver
// failure (e.g. systemctl restart failing) is relayed as a 502 with the
// driver's own error text, never swallowed.
func TestHandleRuntimeAction_DriverExecutionError(t *testing.T) {
	fake := &fakeControlDriver{restartErr: errors.New("systemd: restart ollama.service: Unit not found")}
	withFakeControlDriver(t, fake)
	s := newControlActionTestServer()

	w := doRuntimeAction(t, s, "restart", map[string]string{"driver": "systemd", "identifier": "ollama.service"})

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", w.Code, w.Body.String())
	}
	var resp actionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Error, "Unit not found") {
		t.Fatalf("error = %q, want it to contain the driver's real error text", resp.Error)
	}
}

// TestBuildControlDriver_UnknownDriverErrors verifies the real
// (non-mocked) buildControlDriver rejects an unrecognized driver name
// rather than silently building something.
func TestBuildControlDriver_UnknownDriverErrors(t *testing.T) {
	if _, err := buildControlDriver("kubernetes", "x", ""); err == nil {
		t.Fatal("expected an error for an unknown driver")
	}
}

// TestBuildControlDriver_ProcessSplitsStartCommand verifies the Process
// driver's StartCommand is split into argv the way ProcessDriver.Start
// expects (StartCommand[0] is the binary, the rest are args).
func TestBuildControlDriver_ProcessSplitsStartCommand(t *testing.T) {
	drv, err := buildControlDriver("process", "/var/run/ollama.pid", "/usr/local/bin/ollama serve")
	if err != nil {
		t.Fatalf("buildControlDriver: %v", err)
	}
	pd, ok := drv.(*control.ProcessDriver)
	if !ok {
		t.Fatalf("expected *control.ProcessDriver, got %T", drv)
	}
	if len(pd.StartCommand) != 2 || pd.StartCommand[0] != "/usr/local/bin/ollama" || pd.StartCommand[1] != "serve" {
		t.Fatalf("StartCommand = %v, want [/usr/local/bin/ollama serve]", pd.StartCommand)
	}
}

// TestBuildControlDriver_ProcessSplitsQuotedStartCommand verifies a
// double-quoted segment (e.g. a Windows path with a space) survives as one
// argv token instead of being broken apart by a plain whitespace split.
func TestBuildControlDriver_ProcessSplitsQuotedStartCommand(t *testing.T) {
	drv, err := buildControlDriver("process", "pid", `"C:\Program Files\Ollama\ollama.exe" serve --model "llama 3"`)
	if err != nil {
		t.Fatalf("buildControlDriver: %v", err)
	}
	pd := drv.(*control.ProcessDriver)
	want := []string{`C:\Program Files\Ollama\ollama.exe`, "serve", "--model", "llama 3"}
	if len(pd.StartCommand) != len(want) {
		t.Fatalf("StartCommand = %v, want %v", pd.StartCommand, want)
	}
	for i := range want {
		if pd.StartCommand[i] != want[i] {
			t.Fatalf("StartCommand[%d] = %q, want %q (full: %v)", i, pd.StartCommand[i], want[i], pd.StartCommand)
		}
	}
}

// TestBuildControlDriver_StartCommandRejectedForNonProcessDriver verifies
// the defensive guard: start_command is only ever meaningful for the
// process driver, so any other driver combined with a non-empty
// start_command is rejected rather than silently ignored - this is the only
// enforcement point, since a future caller besides runtimeActionViaAgent
// could otherwise send the mismatched combination unnoticed.
func TestBuildControlDriver_StartCommandRejectedForNonProcessDriver(t *testing.T) {
	if _, err := buildControlDriver("systemd", "ollama.service", "/usr/local/bin/ollama serve"); err == nil {
		t.Fatal("expected an error when start_command is set for a non-process driver")
	}
}

// TestParseDFOutput is a regression test for the disk-space check behind
// handleRuntimeDisk: a real production report had the mesh's pre-pull
// disk-fit gate wave a pull through (host-level disk looked fine) that then
// failed deep into a multi-GB transfer with "no space left on device" -
// because the container's actual storage lived on a different, smaller
// volume than the agent's own host filesystem. This is the parser for the
// `docker exec <container> df -Pk <path>` output that fixes that gap.
func TestParseDFOutput(t *testing.T) {
	// Standard GNU coreutils / busybox `df -Pk` output: a header line plus
	// exactly one data row (guaranteed by -P, the "portable" format).
	out := "Filesystem     1024-blocks      Used Available Capacity Mounted on\n" +
		"overlay          61255492  32000000  26000000      56% /\n"
	free, total, err := parseDFOutput(out)
	if err != nil {
		t.Fatalf("parseDFOutput: %v", err)
	}
	if want := int64(26000000) * 1024; free != want {
		t.Errorf("free = %d, want %d", free, want)
	}
	if want := int64(61255492) * 1024; total != want {
		t.Errorf("total = %d, want %d", total, want)
	}

	// A long device/volume name (common for a Docker named volume, e.g.
	// "/dev/mapper/docker-253:1-1234567-ollama_data") - -P's whole point is
	// a fixed, always-one-line-per-filesystem format, so this must parse
	// exactly like the short-name case above, not wrap onto a second line.
	longName := "Filesystem                                                  1024-blocks      Used Available Capacity Mounted on\n" +
		"/dev/mapper/docker-253:1-1234567-ollama_data                   10000000   4000000   6000000      40% /root/.ollama\n"
	free, total, err = parseDFOutput(longName)
	if err != nil {
		t.Fatalf("parseDFOutput(longName): %v", err)
	}
	if want := int64(6000000) * 1024; free != want {
		t.Errorf("longName free = %d, want %d", free, want)
	}
	if want := int64(10000000) * 1024; total != want {
		t.Errorf("longName total = %d, want %d", total, want)
	}

	if _, _, err := parseDFOutput(""); err == nil {
		t.Error("expected an error for empty df output")
	}
	if _, _, err := parseDFOutput("not df output at all"); err == nil {
		t.Error("expected an error for unparseable df output")
	}
}

// TestHandleRuntimeDisk_NoDriverFallsBackToHostTelemetry verifies the
// non-docker (or unconfigured) case never attempts a docker exec at all -
// it returns whatever host-level disk telemetry this agent already
// collected, exactly like GET /v1/status would, since for a native install
// the host's disk *is* the runtime's disk. Uses a deterministic
// fakeHostCollector (the same seam scheduler_test.go uses) rather than
// asserting on a bare *Server's fallback numbers - snapshot() seeds a live
// one-off Scheduler when none is set, which reads this *test machine's*
// real disk and previously made this test pass locally (Windows: host
// telemetry unimplemented, fields omitted) while failing in CI (Linux:
// real, non-zero numbers) - a platform-dependent assertion, not a real bug.
func TestHandleRuntimeDisk_NoDriverFallsBackToHostTelemetry(t *testing.T) {
	s := newControlActionTestServer()
	sched := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{DiskFreeGB: 220, DiskTotalGB: 1000}}, noRuntimeDetector)
	sched.Seed()
	s.SetScheduler(sched)

	w := doRuntimeAction(t, s, "disk", map[string]string{})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp diskStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != "" {
		t.Errorf("Error = %q, want empty", resp.Error)
	}
	wantFree := int64(220 * 1024 * 1024 * 1024)
	wantTotal := int64(1000 * 1024 * 1024 * 1024)
	if resp.FreeBytes != wantFree || resp.TotalBytes != wantTotal {
		t.Errorf("FreeBytes/TotalBytes = %d/%d, want %d/%d (the fake host collector's fixed values, unaffected by the driver being unconfigured)", resp.FreeBytes, resp.TotalBytes, wantFree, wantTotal)
	}
}
