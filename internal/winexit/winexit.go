// Package winexit provides a single fatal-exit path shared by main.go and
// internal/nodeagent. On Windows, if the process owns its own console (no
// pre-existing shell will remain open after exit - e.g. it was launched by
// double-click, a desktop shortcut, or Explorer), Exit pauses for a keypress
// before terminating so a fatal error is readable instead of vanishing with
// the closing console window. Launching from an existing terminal is
// unaffected: exit codes and log output are unchanged on every platform.
package winexit

import "log"

// Fatalf mirrors log.Fatalf but terminates via Exit instead of os.Exit
// directly, so the pause-on-Windows behavior applies uniformly.
func Fatalf(format string, args ...any) {
	log.Printf(format, args...)
	Exit(1)
}

// Fatal mirrors log.Fatal but terminates via Exit instead of os.Exit
// directly, so the pause-on-Windows behavior applies uniformly.
func Fatal(args ...any) {
	log.Print(args...)
	Exit(1)
}
