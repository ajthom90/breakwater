// Package service runs the Breakwater agent as a Windows service or console process.
package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ajthom90/breakwater/agent/internal/control"
	"github.com/ajthom90/breakwater/agent/internal/enroll"
	"github.com/ajthom90/breakwater/agent/internal/state"
)

// Name is the Windows service name.
const Name = "BreakwaterAgent"

// DisplayName is shown in services.msc.
const DisplayName = "Breakwater Backup Agent"

// Config for running the agent.
type Config struct {
	StateDir    string
	EnrollToken string // if set and not enrolled, enroll on start
	Version     string
	Log         *slog.Logger
	// Console forces interactive mode (also --console flag).
	Console bool
}

// Run starts the agent: enroll if needed, then control loop.
// On Windows without Console, installs as a service handler when launched by SCM.
// On non-Windows (and with Console), runs interactively until SIGINT/SIGTERM.
func Run(cfg Config) error {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if cfg.Version != "" {
		enroll.Version = cfg.Version
	}

	if !cfg.Console && isWindowsService() {
		return runWindowsService(cfg, log)
	}
	return runConsole(cfg, log)
}

func runConsole(cfg Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAgent(ctx, cfg, log)
}

// runAgent is the platform-independent agent body (shared by console + service).
func runAgent(ctx context.Context, cfg Config, log *slog.Logger) error {
	dir, err := state.Open(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	log.Info("state directory", "path", dir.Path)

	if !dir.IsEnrolled() {
		token := cfg.EnrollToken
		if token == "" {
			// Prefer SecureDir-restricted file; migrate/delete legacy HKLM (S4-F2).
			token = readPendingEnrollToken(dir.Path)
		}
		if token == "" {
			return fmt.Errorf("not enrolled: pass --enroll-token, set BWTOKEN, or install with BWTOKEN=")
		}
		log.Info("enrolling…")
		res, err := enroll.Run(ctx, enroll.Options{
			Token:    token,
			StateDir: dir,
			Version:  cfg.Version,
		})
		if err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
		clearPendingEnrollToken(dir.Path) // delete file + registry value, not blank
		log.Info("enrolled", "machine_id", res.MachineID)
	}

	meta, creds, err := dir.LoadIdentity()
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}

	agent := control.New(control.Config{
		State:   dir,
		Meta:    meta,
		Creds:   creds,
		Version: cfg.Version,
		Log:     log,
	})

	// On stop signal, cancel jobs gracefully.
	go func() {
		<-ctx.Done()
		log.Info("shutdown requested; cancelling active jobs")
		agent.Stop()
	}()

	log.Info("control loop starting", "server", meta.ServerAddr, "machine_id", meta.MachineID)
	err = agent.Run(ctx)
	if err != nil && ctx.Err() == nil {
		return err
	}
	// Brief grace for in-flight JobResults after cancel.
	time.Sleep(200 * time.Millisecond)
	log.Info("agent stopped")
	return nil
}
