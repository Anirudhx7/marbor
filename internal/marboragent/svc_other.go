//go:build !windows

package marboragent

func runWindowsServiceIfService(runAgent func(stop <-chan struct{})) (bool, error) {
	return false, nil
}
