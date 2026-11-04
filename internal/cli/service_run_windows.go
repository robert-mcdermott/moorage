//go:build windows

package cli

import (
	"context"

	"golang.org/x/sys/windows/svc"

	"github.com/robert-mcdermott/moorage/internal/service"
)

// runUnderServiceManager runs the server as Windows expects of a service, if that is how it was started.
func runUnderServiceManager(ctx context.Context, run func(context.Context) error) (bool, error) {
	underManager, err := svc.IsWindowsService()
	if err != nil || !underManager {
		return false, err
	}
	h := &serviceHandler{ctx: ctx, run: run}
	if err := svc.Run(service.ServiceName, h); err != nil {
		return true, err
	}
	return true, h.err
}

type serviceHandler struct {
	ctx context.Context
	run func(context.Context) error
	err error
}

// Execute says that the server is running, and stops it when asked. The apps go on.
func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				h.err = <-done
				return false, 0
			}
		case h.err = <-done:
			// A server that stopped by itself has failed, and Windows starts it again.
			status <- svc.Status{State: svc.StopPending}
			if h.err != nil {
				return true, 1
			}
			return false, 0
		}
	}
}
