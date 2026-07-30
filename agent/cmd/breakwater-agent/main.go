// Command breakwater-agent is the Windows agent service.
//
// Modes:
//
//	breakwater-agent --console [--state-dir DIR] [--enroll-token BW1:...]
//	  Interactive process for development and CI (no SCM).
//
//	breakwater-agent
//	  When launched by the Windows Service Control Manager, runs as SYSTEM service.
//	  Outside SCM (or non-Windows), falls back to console mode.
//
//	breakwater-agent --version
//
// Enrollment token may also arrive via BWTOKEN env (MSI property → first-start enroll).
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/ajthom90/breakwater/agent/internal/service"
	"github.com/ajthom90/breakwater/agent/internal/state"
)

// version is overridden at link time: -ldflags "-X main.version=…"
var version = "0.0.1-dev"

func main() {
	// Bare -version / --version before flag parse (CI / MSI probes).
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}

	fs := flag.NewFlagSet("breakwater-agent", flag.ExitOnError)
	showVersion := fs.Bool("version", false, "print version and exit")
	console := fs.Bool("console", false, "run interactively (not as a Windows service)")
	stateDir := fs.String("state-dir", "", "state directory (default: "+state.DefaultStateDir+")")
	enrollToken := fs.String("enroll-token", "", "BW1 enrollment token (or BWTOKEN env)")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Println(version)
		return
	}

	level := slog.LevelInfo
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	token := *enrollToken
	if token == "" {
		token = os.Getenv("BWTOKEN")
	}
	dir := *stateDir
	if dir == "" {
		if v := os.Getenv("BW_STATE_DIR"); v != "" {
			dir = v
		} else {
			dir = state.DefaultStateDir
		}
	}
	if dir != "" && !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}

	err := service.Run(service.Config{
		StateDir:    dir,
		EnrollToken: token,
		Version:     version,
		Log:         log,
		Console:     *console,
	})
	if err != nil {
		log.Error("agent failed", "err", err)
		os.Exit(1)
	}
}
