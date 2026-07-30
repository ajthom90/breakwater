// Command breakwaterd is the Breakwater server (single process, single container).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

var version = "0.0.1-dev"

func main() {
	var (
		dataDir   = flag.String("data", "/data", "data directory (catalog + keys)")
		reposDir  = flag.String("repos", "/repos", "repositories root")
		agentAddr = flag.String("agent-addr", ":9443", "agent gRPC listen address (mTLS)")
		webAddr   = flag.String("web-addr", ":8443", "web/REST listen address")
		hostname  = flag.String("hostname", "breakwater", "server certificate CN / hostname")
		showVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		os.Exit(0)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Error("mkdir data", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*reposDir, 0o700); err != nil {
		log.Error("mkdir repos", "err", err)
		os.Exit(1)
	}
	keysDir := filepath.Join(*dataDir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		log.Error("mkdir keys", "err", err)
		os.Exit(1)
	}

	db, err := catalog.Open(filepath.Join(*dataDir, "catalog.db"))
	if err != nil {
		log.Error("catalog open", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	ks, err := keystore.OpenOrCreate(db, filepath.Join(keysDir, "master.key"))
	if err != nil {
		log.Error("keystore", "err", err)
		os.Exit(1)
	}

	serverID, err := loadOrCreateServerIdentity(keysDir, *hostname)
	if err != nil {
		log.Error("server identity", "err", err)
		os.Exit(1)
	}
	serverFP := serverID.Fingerprint()
	log.Info("server identity ready", "fp", serverFP[:16]+"…", "version", version)

	vm := vault.NewManager(*reposDir)
	defer vm.CloseAll(context.Background())

	enrollSvc := &enroll.Service{
		DB:       db,
		Keystore: ks,
		Vaults:   &vaultAdapter{m: vm},
		ServerFP: serverFP,
		Log:      log,
	}

	gw := agentgw.New(serverID, enrollSvc, log)
	if _, err := gw.Start(*agentAddr); err != nil {
		log.Error("agent gateway", "err", err)
		os.Exit(1)
	}
	defer gw.GracefulStop()

	// Minimal web surface for M1: healthz only (UI arrives M2+).
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			http.Error(w, "catalog unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s\n", version)
	})

	webSrv := &http.Server{
		Addr:              *webAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("web listening", "addr", *webAddr)
		if err := webSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("web serve", "err", err)
			stop()
		}
	}()

	log.Info("breakwaterd ready", "agent", *agentAddr, "web", *webAddr)

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = webSrv.Shutdown(shutdownCtx)
	gw.GracefulStop()
}

type vaultAdapter struct {
	m *vault.Manager
}

func (a *vaultAdapter) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
	v, err := a.m.Create(ctx, repoID, password)
	if err != nil {
		return nil, "", err
	}
	return v.HashingKey(ctx)
}

func loadOrCreateServerIdentity(keysDir, hostname string) (*mtls.Identity, error) {
	certPath := filepath.Join(keysDir, "server.crt")
	keyPath := filepath.Join(keysDir, "server.key")
	if _, err := os.Stat(certPath); err == nil {
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return nil, err
		}
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		return mtls.LoadIdentityFromPEM(certPEM, keyPEM)
	}
	id, err := mtls.GenerateServerIdentity(hostname, []string{hostname, "localhost"}, 10*365*24*time.Hour)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, id.CertPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, id.KeyPEM, 0o600); err != nil {
		return nil, err
	}
	return id, nil
}
