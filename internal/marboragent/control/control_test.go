package control

import (
	"context"
	"errors"
	"testing"
)

// withCommand temporarily replaces the package-level runCommand seam so a
// driver's exec.CommandContext-backed methods can be tested deterministically
// against a canned output/error, mirroring internal/marboragent's withLookPath
// pattern for gpu collectors.
func withCommand(t *testing.T, fn func(ctx context.Context, name string, args ...string) (string, error)) {
	t.Helper()
	old := runCommand
	runCommand = fn
	t.Cleanup(func() { runCommand = old })
}

func withLookPathFound(t *testing.T, found bool) {
	t.Helper()
	old := lookPath
	if found {
		lookPath = func(string) (string, error) { return "/usr/bin/tool", nil }
	} else {
		lookPath = func(string) (string, error) { return "", errors.New("not found") }
	}
	t.Cleanup(func() { lookPath = old })
}

func TestSystemdDriverStartStopRestart(t *testing.T) {
	var gotArgs []string
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		gotArgs = append([]string{name}, args...)
		return "", nil
	})
	d := &SystemdDriver{Unit: "ollama.service"}
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if want := []string{"systemctl", "start", "ollama.service"}; !equalSlices(gotArgs, want) {
		t.Errorf("Start args = %v, want %v", gotArgs, want)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := d.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
}

func TestSystemdDriverStatusActive(t *testing.T) {
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		return "active\n", nil
	})
	d := &SystemdDriver{Unit: "ollama.service"}
	st, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running || st.Detail != "active" {
		t.Errorf("Status = %+v, want Running=true Detail=active", st)
	}
}

func TestSystemdDriverStatusInactiveIsNotAnError(t *testing.T) {
	// systemctl is-active exits non-zero for "inactive" - the driver must
	// not treat that non-zero exit as a query failure.
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		return "inactive\n", errors.New("exit status 3")
	})
	d := &SystemdDriver{Unit: "ollama.service"}
	st, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status returned error for a valid inactive state: %v", err)
	}
	if st.Running {
		t.Error("expected Running=false for inactive state")
	}
	if st.Detail != "inactive" {
		t.Errorf("Detail = %q, want inactive", st.Detail)
	}
}

func TestSystemdDriverValidateMissingUnit(t *testing.T) {
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		return "", nil
	})
	d := &SystemdDriver{Unit: "nonexistent.service"}
	if err := d.Validate(context.Background()); err == nil {
		t.Error("expected Validate to fail when list-unit-files returns nothing")
	}
}

func TestDockerDriverStatusRunning(t *testing.T) {
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		return "true\n", nil
	})
	d := &DockerDriver{Container: "ollama"}
	st, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running {
		t.Error("expected Running=true")
	}
}

func TestDockerDriverValidateContainerNotFound(t *testing.T) {
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		return "Error: No such object: ollama", errors.New("exit status 1")
	})
	d := &DockerDriver{Container: "ollama"}
	if err := d.Validate(context.Background()); err == nil {
		t.Error("expected Validate to fail when docker inspect errors")
	}
}

func TestProcessDriverStartRequiresCommand(t *testing.T) {
	d := &ProcessDriver{PIDFile: "/tmp/does-not-matter.pid"}
	if err := d.Start(context.Background()); err == nil {
		t.Error("expected Start to fail with no StartCommand configured")
	}
}

func TestProcessDriverLogsUnsupported(t *testing.T) {
	d := &ProcessDriver{}
	if _, err := d.Logs(context.Background(), 10); err == nil {
		t.Error("expected Logs to always error for ProcessDriver")
	}
}

func TestWindowsServiceDriverStatusParsesState(t *testing.T) {
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		return "SERVICE_NAME: ollama\n        STATE              : 4  RUNNING\n", nil
	})
	d := &WindowsServiceDriver{Service: "ollama"}
	st, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running {
		t.Errorf("expected Running=true, Detail=%q", st.Detail)
	}
}

func TestLaunchdDriverStatusNotLoaded(t *testing.T) {
	withCommand(t, func(ctx context.Context, name string, args ...string) (string, error) {
		return "", errors.New("Could not find service")
	})
	d := &LaunchdDriver{Label: "com.example.ollama"}
	if _, err := d.Status(context.Background()); err == nil {
		t.Error("expected Status to error when label isn't loaded")
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
