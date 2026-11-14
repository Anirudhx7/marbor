package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		if got, _ := tasklistNamesMarbor(tc.out, tc.pid); got != tc.want {
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
			if got, reason := pidIdentityConfirmed(tc.goos, 42, run); got != tc.want || (!got && reason == "") {
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
		outcome := stopPidfileProcess(path, goos, failing, &stdout, &stderr)
		if outcome != pidUnverified {
			t.Fatalf("%s: outcome=%v, want pidUnverified", goos, outcome)
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
	outcome := stopPidfileProcess(filepath.Join(t.TempDir(), "missing.pid"), "linux", nil, &out, &out)
	if outcome != pidNone {
		t.Errorf("got outcome=%v, want pidNone", outcome)
	}
}

func TestPidIdentityConfirmed_LinuxProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		return
	}
	// The test binary's argv[0] is not exactly marbor.
	if ok, reason := pidIdentityConfirmed("linux", os.Getpid(), nil); ok || reason == "" {
		t.Errorf("own test pid: ok=%v reason=%q, want false with a reason", ok, reason)
	}
	ok, reason := pidIdentityConfirmed("linux", deadPid(t), nil)
	if ok || !strings.Contains(reason, "/proc/") {
		t.Errorf("nonexistent pid: ok=%v reason=%q, want false naming /proc", ok, reason)
	}
}

func TestTasklistNamesMarbor_ParseError(t *testing.T) {
	if ok, err := tasklistNamesMarbor("\"unterminated,1234\n", 1234); ok || err == nil {
		t.Errorf("ok=%v err=%v, want false with an error", ok, err)
	}
	if ok, err := tasklistNamesMarbor("", 1); ok || err == nil {
		t.Errorf("empty output: ok=%v err=%v, want false with an error", ok, err)
	}
}

func TestUninstallSummary(t *testing.T) {
	cases := []struct {
		name    string
		service bool
		outcome pidfileOutcome
		want    []string
		notWant []string
	}{
		{"nothing", false, pidNone, []string{"Nothing to uninstall"}, nil},
		{"stopped", false, pidStopped, []string{"has been uninstalled"}, []string{"may still be running"}},
		{"unverified only", false, pidUnverified, []string{"Nothing was uninstalled", "may still be running"}, []string{"Nothing to uninstall"}},
		{"stop failed only", false, pidStopFailed, []string{"Nothing was uninstalled", "could not be stopped"}, []string{"Nothing to uninstall"}},
		{"service plus unverified", true, pidUnverified, []string{"has been uninstalled", "may still be running"}, []string{"Nothing"}},
		{"service plus stop failed", true, pidStopFailed, []string{"has been uninstalled", "could not be stopped"}, []string{"Nothing"}},
		{"incomplete only", false, pidStoppedIncomplete, []string{"Uninstall is incomplete", "not confirmed stopped", "pidfile"}, []string{"has been uninstalled", "Nothing"}},
		{"service plus incomplete", true, pidStoppedIncomplete, []string{"has been uninstalled", "not confirmed stopped"}, []string{"Nothing"}},
	}
	for _, tc := range cases {
		got := uninstallSummary(tc.service, tc.outcome)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q missing %q", tc.name, got, w)
			}
		}
		for _, w := range tc.notWant {
			if strings.Contains(got, w) {
				t.Errorf("%s: %q must not contain %q", tc.name, got, w)
			}
		}
	}
}

// TestHelperSleep is not a test: when re-executed with the helper variable
// set it just sleeps, giving the tests a harmless child process to stop or
// time out on.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("MARBOR_TEST_HELPER_SLEEP") == "" {
		return
	}
	time.Sleep(30 * time.Second)
}

func TestStopPidfileProcess_ConfirmedStopsChild(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSleep")
	cmd.Env = append(os.Environ(), "MARBOR_TEST_HELPER_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	goos, out := "darwin", "marbor\n"
	if runtime.GOOS == "windows" {
		goos, out = "windows", "\"marbor.exe\",\""+strconv.Itoa(pid)+"\"\r\n"
	}
	run := func(string, ...string) ([]byte, error) { return []byte(out), nil }
	path := writePidfile(t, strconv.Itoa(pid))

	var stdout, stderr bytes.Buffer
	if got := stopPidfileProcess(path, goos, run, &stdout, &stderr); got != pidStopped {
		t.Fatalf("outcome = %v, want pidStopped (stderr: %s)", got, stderr.String())
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit after being stopped")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pidfile should be removed, stat err = %v", err)
	}
}

func stubStopHooks(t *testing.T, term func(int, string, io.Writer) bool, wait func(int, time.Duration) bool) {
	t.Helper()
	oldT, oldW, oldD := terminateFn, waitExitFn, stopWaitTimeout
	terminateFn, waitExitFn, stopWaitTimeout = term, wait, time.Millisecond
	t.Cleanup(func() { terminateFn, waitExitFn, stopWaitTimeout = oldT, oldW, oldD })
}

func marborComm(string, ...string) ([]byte, error) { return []byte("marbor\n"), nil }

// On Linux the process is identified by /proc cmdline: a process whose
// command name is not marbor (this test binary) is never signalled and the
// pidfile is kept.
func TestStopPidfileProcess_LinuxNonMarborNotSignalled(t *testing.T) {
	if runtime.GOOS != "linux" {
		return
	}
	stubStopHooks(t, func(int, string, io.Writer) bool {
		t.Error("terminate must not be called for a non-marbor process")
		return true
	}, func(int, time.Duration) bool { return true })
	path := writePidfile(t, strconv.Itoa(os.Getpid()))
	var stdout, stderr bytes.Buffer
	if got := stopPidfileProcess(path, "linux", nil, &stdout, &stderr); got != pidUnverified {
		t.Fatalf("outcome = %v, want pidUnverified", got)
	}
	if !strings.Contains(stderr.String(), "its command name is not marbor") {
		t.Errorf("reason missing: %q", stderr.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("pidfile must be kept: %v", err)
	}
}

func TestStopPidfileProcess_TerminateFailureKeepsPidfile(t *testing.T) {
	stubStopHooks(t, func(int, string, io.Writer) bool { return false }, func(int, time.Duration) bool { return true })
	path := writePidfile(t, strconv.Itoa(os.Getpid()))
	var stdout, stderr bytes.Buffer
	if got := stopPidfileProcess(path, "darwin", marborComm, &stdout, &stderr); got != pidStopFailed {
		t.Fatalf("outcome = %v, want pidStopFailed", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("pidfile must be kept when the stop failed: %v", err)
	}
}

func TestStopPidfileProcess_StillAliveIsIncomplete(t *testing.T) {
	stubStopHooks(t, func(int, string, io.Writer) bool { return true }, func(int, time.Duration) bool { return false })
	path := writePidfile(t, strconv.Itoa(os.Getpid()))
	var stdout, stderr bytes.Buffer
	if got := stopPidfileProcess(path, "darwin", marborComm, &stdout, &stderr); got != pidStoppedIncomplete {
		t.Fatalf("outcome = %v, want pidStoppedIncomplete", got)
	}
	if !strings.Contains(stderr.String(), "did not exit") {
		t.Errorf("warning missing: %q", stderr.String())
	}
}

func TestStopPidfileProcess_PidfileNotRemovableIsIncomplete(t *testing.T) {
	if runtime.GOOS == "windows" {
		return // directory permission bits do not block removal on Windows
	}
	stubStopHooks(t, func(int, string, io.Writer) bool { return true }, func(int, time.Duration) bool { return true })
	dir := t.TempDir()
	path := filepath.Join(dir, "marbor.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if os.Getuid() == 0 {
		return // root ignores directory permissions
	}
	var stdout, stderr bytes.Buffer
	if got := stopPidfileProcess(path, "darwin", marborComm, &stdout, &stderr); got != pidStoppedIncomplete {
		t.Fatalf("outcome = %v, want pidStoppedIncomplete", got)
	}
	if !strings.Contains(stderr.String(), "could not remove") {
		t.Errorf("warning missing: %q", stderr.String())
	}
}

func TestTerminatePid_FailureBranches(t *testing.T) {
	dead := deadPid(t)
	var stderr bytes.Buffer
	if runtime.GOOS == "windows" {
		// FindProcess fails for a pid that no longer exists.
		if terminatePid(dead, "windows", &stderr) || !strings.Contains(stderr.String(), "could not") {
			t.Errorf("windows: want false with a warning, got %q", stderr.String())
		}
		return
	}
	if terminatePid(dead, "linux", &stderr) || !strings.Contains(stderr.String(), "could not signal") {
		t.Errorf("non-windows signal failure: got %q", stderr.String())
	}
	stderr.Reset()
	if terminatePid(dead, "windows", &stderr) || !strings.Contains(stderr.String(), "could not stop") {
		t.Errorf("kill fallback failure: got %q", stderr.String())
	}
}

func TestRunCommandWithTimeout_KillsHungCommand(t *testing.T) {
	t.Setenv("MARBOR_TEST_HELPER_SLEEP", "1")
	start := time.Now()
	_, err := runCommandWithTimeout(50*time.Millisecond, os.Args[0], "-test.run=TestHelperSleep")
	if err == nil {
		t.Fatal("expected an error from a command that outlives the timeout")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("command was not cut off promptly: %v", elapsed)
	}
}
