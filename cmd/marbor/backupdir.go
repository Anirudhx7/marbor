package main

import (
	"log"
	"path/filepath"
	"strings"

	"github.com/Anirudhx7/marbor/internal/config"
)

// absBackupDir resolves a backup directory that was seeded from the
// environment, or stored by an earlier version, against the current working
// directory. Older versions accepted relative paths and resolved them the same
// way at every use, so this keeps such an install pointed at the same place
// while config.Validate now requires an absolute path.
func absBackupDir(dir string) string {
	// Only a plain relative path is resolved. A rooted, drive-qualified or
	// device-namespace value is left as written, so the validator sees (and
	// refuses) exactly what was stored instead of a mangled absolute form.
	if dir == "" || filepath.IsAbs(dir) || filepath.VolumeName(dir) != "" || strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "\\") {
		return dir
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// storedBackupDir returns the persisted backup directory when it passes the
// same check a Settings change must pass, and fallback otherwise, so a stale or
// hand-edited row cannot stop marbor from starting or point backups somewhere
// the check would refuse.
func storedBackupDir(stored, fallback string) string {
	dir := absBackupDir(stored)
	if err := config.ValidateBackupTargetDir(dir); err != nil {
		log.Printf("ignoring the stored backup_target_dir: it %v; using the default backup directory", err)
		return fallback
	}
	return dir
}
