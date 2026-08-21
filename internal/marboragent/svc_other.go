//go:build !windows

package marboragent

func runWindowsServiceIfService(runAgent func()) (bool, error) {
	return false, nil
}
