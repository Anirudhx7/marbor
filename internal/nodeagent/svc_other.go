//go:build !windows

package nodeagent

func runWindowsServiceIfService(runAgent func()) (bool, error) {
	return false, nil
}
