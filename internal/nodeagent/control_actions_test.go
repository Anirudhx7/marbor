package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent/control"
)

// fakeControlDriver satisfies control.ControlDriver for tests, letting a
// test observe which verb was called and control the returned error without
// depending on any real subprocess execution.
type fakeControlDriver struct {
	startErr, stopErr, restartErr error
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
func (f *fakeControlDriver) Logs(ctx context.Context, lines int) ([]string, error) { return nil, nil }
func (f *fakeControlDriver) Validate(ctx context.Context) error                    { return nil }

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
