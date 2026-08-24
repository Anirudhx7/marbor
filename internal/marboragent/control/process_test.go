package control

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadPIDFileRejectsNonPositivePID is the P103 regression: pid<=0 has
// special meaning to the OS's kill/signal syscalls (0 signals the whole
// process group, -1 signals every process the caller can signal) - a
// corrupted/misconfigured pid file value must never reach Stop/Restart's
// proc.Kill()/proc.Signal() unguarded.
func TestReadPIDFileRejectsNonPositivePID(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"valid positive pid", "12345", false},
		{"zero", "0", true},
		{"negative", "-1", true},
		{"non-numeric", "not-a-pid", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test.pid")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("setup WriteFile: %v", err)
			}
			_, err := readPIDFile(path)
			if tc.wantErr && err == nil {
				t.Errorf("readPIDFile(%q) = nil error, want error", tc.content)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("readPIDFile(%q) = %v, want nil error", tc.content, err)
			}
		})
	}
}
