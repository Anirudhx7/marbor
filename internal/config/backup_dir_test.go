package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateBackupTargetDir(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "backups")
	good := []string{"", abs, filepath.Join(abs, "a b", "c.d")}
	for _, dir := range good {
		if err := ValidateBackupTargetDir(dir); err != nil {
			t.Errorf("ValidateBackupTargetDir(%q) = %v, want nil", dir, err)
		}
	}

	root := string(filepath.Separator)
	bad := map[string]string{
		"relative":          "backups",
		"dot relative":      "." + root + "backups",
		"parent traversal":  abs + root + ".." + root + "other",
		"leading traversal": root + ".." + root + "etc",
		"filesystem root":   root,
		"nul byte":          abs + "\x00evil",
		"newline":           abs + "\nevil",
		"escape character":  abs + "\x1b[2J",
		"tab":               abs + "\tx",
		"too long":          root + strings.Repeat("a", maxBackupDirLen),
		"only spaces":       "   ",
	}
	if runtime.GOOS == "windows" {
		bad["drive root"] = `C:\`
		bad["drive relative"] = `C:backups`
		bad["rooted without drive"] = `\backups`
		bad["device namespace"] = `\\.\C:\backups`
		bad["extended prefix"] = `\\?\C:\backups`
		bad["unc device"] = `\\.\UNC\srv\share`
		bad["reserved device name"] = `C:\backups\CON`
		bad["reserved name with extension"] = `C:\backups\nul.txt`
		bad["serial device"] = `C:\backups\com1`
		bad["stream syntax"] = `C:\backups:stream`
		bad["trailing dot"] = `C:\backups.`
		bad["trailing space element"] = `C:\backups \x`
		bad["wildcard"] = `C:\back*ups`
	}
	for name, dir := range bad {
		if err := ValidateBackupTargetDir(dir); err == nil {
			t.Errorf("%s: ValidateBackupTargetDir(%q) = nil, want an error", name, dir)
		}
	}
}

func TestValidateRejectsBadBackupTargetDirAndCleansGoodOne(t *testing.T) {
	cfg := Config{}
	cfg.Backup.TargetDir = "backups"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "backup.target_dir") {
		t.Fatalf("Validate() with a relative target_dir = %v, want a backup.target_dir error", err)
	}

	base := t.TempDir()
	cfg = Config{}
	cfg.Backup.TargetDir = base + string(filepath.Separator) + "x" + string(filepath.Separator) + string(filepath.Separator)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with a clean absolute target_dir = %v", err)
	}
	if want := filepath.Join(base, "x"); cfg.Backup.TargetDir != want {
		t.Errorf("TargetDir = %q, want the cleaned %q", cfg.Backup.TargetDir, want)
	}
}
