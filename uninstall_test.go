package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// deadPid starts a short-lived copy of the test binary, waits for it to exit,
// and returns its (now unused) pid.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=NONE")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait helper: %v", err)
	}
	return pid
}

func writePidfile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "marbor.pid")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	return path
}

func TestReadRunningPidfile_OwnPidIsRunning(t *testing.T) {
	path := writePidfile(t, strconv.Itoa(os.Getpid())+"\n")
	pid, ok := readRunningPidfile(path)
	if !ok || pid != os.Getpid() {
		t.Fatalf("readRunningPidfile = (%d, %v), want (%d, true)", pid, ok, os.Getpid())
	}
}

func TestReadRunningPidfile_Rejects(t *testing.T) {
	cases := map[string]string{
		"garbage":  "not-a-pid",
		"empty":    "",
		"zero":     "0",
		"negative": "-5",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if pid, ok := readRunningPidfile(writePidfile(t, content)); ok {
				t.Fatalf("readRunningPidfile = (%d, true), want false", pid)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		if _, ok := readRunningPidfile(filepath.Join(t.TempDir(), "absent.pid")); ok {
			t.Fatal("readRunningPidfile reported ok for a missing file")
		}
	})
}

func TestReadRunningPidfile_DeadPidIsNotRunning(t *testing.T) {
	path := writePidfile(t, strconv.Itoa(deadPid(t)))
	if pid, ok := readRunningPidfile(path); ok {
		t.Fatalf("readRunningPidfile = (%d, true) for an exited process, want false", pid)
	}
}

func TestWaitForProcessExit_DeadPidReturnsQuickly(t *testing.T) {
	start := time.Now()
	if !waitForProcessExit(deadPid(t), 5*time.Second) {
		t.Fatal("waitForProcessExit = false for an exited process, want true")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waitForProcessExit took %v for an exited process", elapsed)
	}
}

func TestWaitForProcessExit_LivePidTimesOut(t *testing.T) {
	if waitForProcessExit(os.Getpid(), 250*time.Millisecond) {
		t.Fatal("waitForProcessExit = true for the running test process, want false")
	}
}
