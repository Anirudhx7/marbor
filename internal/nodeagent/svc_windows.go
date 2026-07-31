//go:build windows

package nodeagent

import (
	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent/service"
	"golang.org/x/sys/windows/svc"
)

func runWindowsServiceIfService(runAgent func()) (bool, error) {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false, err
	}
	err = svc.Run(service.Name, &agentWindowsService{runAgent: runAgent})
	return true, err
}

type agentWindowsService struct {
	runAgent func()
}

func (s *agentWindowsService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	go s.runAgent()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}
