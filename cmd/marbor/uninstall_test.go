package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

func TestPsCommNamesMarbor(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"marbor\n", true},
		{"/usr/local/bin/marbor\n", true},
		{"  /opt/bin/marbor  \n", true},
		{"marbor-agent\n", false},
		{"sshd\n", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := psCommNamesMarbor(tc.out); got != tc.want {
			t.Errorf("psCommNamesMarbor(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

func TestCmdlineNamesMarbor(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain", "marbor\x00serve\x00", true},
		{"absolute path", "/usr/local/bin/marbor\x00", true},
		{"agent rejected", "/usr/local/bin/marbor-agent\x00--port\x009200\x00", false},
		{"other", "sshd\x00-D\x00", false},
		{"empty", "", false},
		{"leading NUL", "\x00marbor", false},
	}
	for _, tc := range cases {
		if got := cmdlineNamesMarbor([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: cmdlineNamesMarbor(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestTasklistNamesMarbor(t *testing.T) {
	cases := []struct {
		name string
		out  string
		pid  int
		want bool
	}{
		{"match", "\"marbor.exe\",\"1234\",\"Console\",\"1\",\"20,000 K\"\r\n", 1234, true},
		{"case-insensitive", "\"MARBOR.EXE\",\"1234\",\"Console\",\"1\",\"20,000 K\"\r\n", 1234, true},
		{"agent", "\"marbor-agent.exe\",\"1234\",\"Console\"\r\n", 1234, false},
		{"notmarbor.exe", "\"notmarbor.exe\",\"1234\",\"Console\"\r\n", 1234, false},
		{"pid mismatch", "\"marbor.exe\",\"999\",\"Console\"\r\n", 1234, false},
		{"only first row counts", "\"notepad.exe\",\"1234\"\r\n\"marbor.exe\",\"1234\"\r\n", 1234, false},
		{"english no match", "INFO: No tasks are running which match the specified criteria.\r\n", 1234, false},
		{"localized info", "INFORMATION: Es werden keine Aufgaben ausgefuehrt, die den angegebenen Kriterien entsprechen.\r\n", 1234, false},
		{"leading blank line", "\r\n\"marbor.exe\",\"1234\",\"Console\"\r\n", 1234, true},
		{"empty", "", 1234, false},
	}
	for _, tc := range cases {
		if got := tasklistNamesMarbor(tc.out, tc.pid); got != tc.want {
			t.Errorf("%s: tasklistNamesMarbor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPidIdentityConfirmed(t *testing.T) {
	boom := errors.New("boom")
	const psArgs = "-p 42 -o comm="
	const tlArgs = "/FI PID eq 42 /FO CSV /NH"
	cases := []struct {
		name     string
		goos     string
		out      string
		err      error
		want     bool
		wantName string
		wantArgs string
	}{
		{"unknown OS", "plan9", "marbor\n", nil, false, "", ""},
		{"darwin error", "darwin", "", boom, false, "ps", psArgs},
		{"darwin marbor", "darwin", "/usr/local/bin/marbor\n", nil, true, "ps", psArgs},
		{"darwin other", "darwin", "bash\n", nil, false, "ps", psArgs},
		{"windows error", "windows", "", boom, false, "tasklist", tlArgs},
		{"windows marbor", "windows", "\"marbor.exe\",\"42\",\"Console\"\r\n", nil, true, "tasklist", tlArgs},
		{"windows other", "windows", "\"chrome.exe\",\"42\"\r\n", nil, false, "tasklist", tlArgs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotName string
			var gotArgs []string
			run := func(name string, args ...string) ([]byte, error) {
				gotName, gotArgs = name, args
				return []byte(tc.out), tc.err
			}
			if got := pidIdentityConfirmed(tc.goos, 42, run); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			if gotName != tc.wantName || strings.Join(gotArgs, " ") != tc.wantArgs {
				t.Errorf("command = %s %v, want %s %s", gotName, gotArgs, tc.wantName, tc.wantArgs)
			}
		})
	}
}

// A live PID that cannot be verified as marbor (here the test process itself)
// must never be signalled, and its pidfile must be kept. Without the guard,
// SIGTERM would hit this very test process.
func TestStopPidfileProcess_UnconfirmedIsNotSignalled(t *testing.T) {
	path := writePidfile(t, strconv.Itoa(os.Getpid()))
	failing := func(string, ...string) ([]byte, error) { return nil, errors.New("tool missing") }

	for _, goos := range []string{"darwin", "windows", "plan9"} {
		var stdout, stderr bytes.Buffer
		stopped, unverified := stopPidfileProcess(path, goos, failing, &stdout, &stderr)
		if stopped || !unverified {
			t.Fatalf("%s: stopped=%v unverified=%v, want false/true", goos, stopped, unverified)
		}
		if !strings.Contains(stderr.String(), "could not be verified as marbor") || !strings.Contains(stderr.String(), "nothing was signalled") {
			t.Errorf("%s: warning text missing, got %q", goos, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: must not announce stopping, got %q", goos, stdout.String())
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: pidfile must be kept when identity is unconfirmed: %v", goos, err)
		}
	}
}

func TestStopPidfileProcess_NoPidfile(t *testing.T) {
	var out bytes.Buffer
	stopped, unverified := stopPidfileProcess(filepath.Join(t.TempDir(), "missing.pid"), "linux", nil, &out, &out)
	if stopped || unverified {
		t.Errorf("got stopped=%v unverified=%v, want false/false", stopped, unverified)
	}
}
