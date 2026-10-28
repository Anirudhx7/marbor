package config

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode"
)

// maxBackupDirLen caps backup.target_dir well below any platform path limit,
// so an absurd value is refused here rather than failing deep in the filesystem.
const maxBackupDirLen = 4096

// ValidateBackupTargetDir checks backup.target_dir where it enters the
// configuration. The empty string is valid (backups are then reported as not
// configured). Anything else must be a plain absolute directory path: no
// control characters or NUL bytes, no ".." element (a path that climbs out of
// where it starts is never what an operator means), not a filesystem or volume
// root, and on Windows not a device or extended-length (\.\ or \?\) path or
// a drive-relative one. Relative paths are refused because they resolve
// against whatever directory marbor was started from.
//
// This is a boundary check on what the operator typed. The files marbor
// creates inside the directory are guarded separately (strict file names, no
// following of symlinks, exclusive creation).
func ValidateBackupTargetDir(dir string) error {
	if dir == "" {
		return nil
	}
	if len(dir) > maxBackupDirLen {
		return errors.New("is too long")
	}
	if strings.TrimSpace(dir) != dir {
		return errors.New("must not start or end with whitespace")
	}
	for _, r := range dir {
		if unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	if strings.HasPrefix(dir, `\.\`) || strings.HasPrefix(dir, `\?\`) || strings.HasPrefix(dir, `//./`) || strings.HasPrefix(dir, `//?/`) {
		return errors.New("must not be a device or extended-length path")
	}
	if !filepath.IsAbs(dir) {
		return errors.New("must be an absolute path")
	}
	for _, elem := range strings.FieldsFunc(dir, func(r rune) bool { return r == '/' || r == '\\' }) {
		if elem == ".." {
			return errors.New(`must not contain a ".." element`)
		}
	}
	cleaned := filepath.Clean(dir)
	if cleaned == filepath.VolumeName(cleaned)+string(filepath.Separator) {
		return errors.New("must not be a filesystem root")
	}
	return nil
}
