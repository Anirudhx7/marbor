package config

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// maxBackupDirLen caps backup.target_dir well below any platform path limit,
// so an absurd value is refused here rather than failing deep in the filesystem.
const maxBackupDirLen = 4096

// windowsReservedNames are the device names Windows resolves in every directory,
// with or without an extension (CON, nul.txt, ...).
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// ValidateBackupTargetDir checks backup.target_dir where it enters the
// configuration. The empty string is valid (backups are then reported as not
// configured). Anything else must be a plain absolute directory path: no
// control characters or NUL bytes, no ".." element (a path that climbs out of
// where it starts is never what an operator means), and not a filesystem or
// volume root. On Windows it must also not be a device or extended-length
// (\\.\ or \\?\) path, and no element may be a reserved device name, name an
// NTFS stream, use a character Windows forbids, or end in a dot or space (Windows
// silently drops those, so two spellings would name one directory). Relative
// paths are refused because they resolve against whatever directory marbor was
// started from.
//
// This is a boundary check on what the operator typed. It says nothing about
// what the directory is on disk (it may be a symlink or a mount); the files
// marbor creates inside it are guarded separately (strict file names, no
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
	windows := runtime.GOOS == "windows"
	if windows && isWindowsDevicePath(dir) {
		return errors.New("must not be a device or extended-length path")
	}
	if !filepath.IsAbs(dir) {
		return errors.New("must be an absolute path")
	}
	rest := strings.TrimPrefix(dir, filepath.VolumeName(dir))
	for _, elem := range strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' }) {
		if elem == ".." {
			return errors.New(`must not contain a ".." element`)
		}
		if windows {
			if err := validateWindowsElement(elem); err != nil {
				return err
			}
		}
	}
	cleaned := filepath.Clean(dir)
	if cleaned == filepath.VolumeName(cleaned)+string(filepath.Separator) {
		return errors.New("must not be a filesystem root")
	}
	return nil
}

func isWindowsDevicePath(dir string) bool {
	for _, prefix := range []string{`\\.\`, `\\?\`, "//./", "//?/"} {
		if strings.HasPrefix(dir, prefix) {
			return true
		}
	}
	return false
}

func validateWindowsElement(elem string) error {
	if elem == "." {
		return nil
	}
	if strings.ContainsAny(elem, `<>:"|?*`) {
		return errors.New(`must not contain any of < > : " | ? * (a colon names an NTFS stream)`)
	}
	if strings.HasSuffix(elem, ".") || strings.HasSuffix(elem, " ") {
		return errors.New("must not have a path element ending in a dot or a space")
	}
	stem, _, _ := strings.Cut(elem, ".")
	if windowsReservedNames[strings.ToUpper(strings.TrimRight(stem, " "))] {
		return errors.New("must not contain a reserved Windows device name")
	}
	return nil
}
