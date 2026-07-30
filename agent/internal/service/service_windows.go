//go:build windows

package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
	"golang.org/x/sys/windows/svc/eventlog"
)

func isWindowsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

func runWindowsService(cfg Config, log *slog.Logger) error {
	// Prefer event log; fall back to slog if registration failed (unsigned MVP).
	elog, err := eventlog.Open(Name)
	if err != nil {
		log.Warn("eventlog open failed; using stderr only", "err", err)
		return svc.Run(Name, &winService{cfg: cfg, log: log})
	}
	defer elog.Close()
	return svc.Run(Name, &winService{cfg: cfg, log: log, elog: elog})
}

// winService implements svc.Handler.
//
// UNTESTED ON WINDOWS until first real CI/VM run — verify:
//   - service Start/Stop/Shutdown via sc.exe / services.msc
//   - graceful job cancellation on Stop (JobResult sent before exit)
//   - auto-start delayed after reboot
//   - event-log source BreakwaterAgent receives start/stop events
type winService struct {
	cfg  Config
	log  *slog.Logger
	elog *eventlog.Log
}

func (m *winService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	m.info(1, "Breakwater agent starting")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runAgent(ctx, m.cfg, m.log)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepts}
	m.info(2, "Breakwater agent running")

loop:
	for {
		select {
		case err := <-errCh:
			if err != nil {
				m.error(3, fmt.Sprintf("agent exited: %v", err))
				errno = 1
			}
			break loop
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				m.info(4, "stop/shutdown requested")
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				// Wait for agent body (bounded).
				select {
				case err := <-errCh:
					if err != nil && err != context.Canceled {
						m.error(5, fmt.Sprintf("agent stop: %v", err))
					}
				case <-time.After(30 * time.Second):
					m.error(6, "agent stop timed out after 30s")
					errno = 1
				}
				break loop
			default:
				m.info(7, fmt.Sprintf("unexpected service control %d", c.Cmd))
			}
		}
	}
	changes <- svc.Status{State: svc.Stopped}
	return
}

func (m *winService) info(eid uint32, msg string) {
	m.log.Info(msg)
	if m.elog != nil {
		_ = m.elog.Info(eid, msg)
	}
}

func (m *winService) error(eid uint32, msg string) {
	m.log.Error(msg)
	if m.elog != nil {
		_ = m.elog.Error(eid, msg)
	}
}

// Ensure debug.Console is referenced if we add a debug path later.
var _ = debug.Run
