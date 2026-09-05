//go:build windows

package marboragent

import (
	"time"

	"github.com/Anirudhx7/marbor/internal/marboragent/service"
	"golang.org/x/sys/windows/svc"
)

func runWindowsServiceIfService(runAgent func(stop <-chan struct{})) (bool, error) {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false, err
	}
	err = svc.Run(service.Name, &agentWindowsService{runAgent: runAgent})
	return true, err
}

type agentWindowsService struct {
	runAgent func(stop <-chan struct{})
}

// gracefulStopTimeout bounds how long Execute waits for runAgent to actually
// return (scheduler context canceled, HTTP server Shutdown complete) before
// reporting Stopped anyway - a hung shutdown must not hang the whole SCM
// stop request indefinitely.
const gracefulStopTimeout = 10 * time.Second

func (s *agentWindowsService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.runAgent(stop)
		close(done)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			// Signal runAgent to cancel the scheduler context and
			// gracefully Shutdown the HTTP server, instead of letting the
			// process die mid-flight (e.g. mid-model-pull proxy) the moment
			// StopPending is reported.
			close(stop)
			select {
			case <-done:
			case <-time.After(gracefulStopTimeout):
			}
			changes <- svc.Status{State: svc.Stopped}
			return false, 0
		}
	}
	return false, 0
}
