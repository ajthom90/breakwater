// Command breakwaterd is the Breakwater server (single process, single container).
package main

import (
	"context"
	"crypto/tls"
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
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/ajthom90/breakwater/server/internal/web"
)

var version = "0.0.1-dev"

func main() {
	var (
		dataDir   = flag.String("data", "/data", "data directory (catalog + keys + kopia config/cache)")
		reposDir  = flag.String("repos", "/repos", "repositories root (blob storage)")
		agentAddr = flag.String("agent-addr", ":9443", "agent gRPC listen address (mTLS)")
		webAddr   = flag.String("web-addr", ":8443", "web/REST listen address (HTTPS)")
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

	vm := vault.NewManager(*reposDir, *dataDir)
	defer vm.CloseAll(context.Background())

	auditor := audit.NewWriter(db)

	// Per-repo job serialization (R2-2 / R3-6). All vault Open/Close/prune for an
	// existing repo must go through leases from these locks (enrollment Create is
	// the documented exception — brand-new repo, no concurrent jobs).
	repoLocks := scheduler.NewRepoLocks()
	eventHub := scheduler.NewEventHub()
	jobEngine := scheduler.NewEngine(db, repoLocks, log)
	jobEngine.Events = eventHub
	// S2-F5: orphaned running rows from a previous process cannot resume.
	if err := jobEngine.RecoverOnStartup(context.Background()); err != nil {
		log.Error("job recovery", "err", err)
		os.Exit(1)
	}
	controlReg := agentgw.NewRegistry(log)

	apiToken, err := web.LoadOrCreateAPIToken(*dataDir, log)
	if err != nil {
		log.Error("api token", "err", err)
		os.Exit(1)
	}

	enrollSvc := &enroll.Service{
		DB:       db,
		Keystore: ks,
		Vaults:   &vaultAdapter{m: vm},
		ServerFP: serverFP,
		Log:      log,
	}

	gw := agentgw.New(serverID, enrollSvc, log)
	gw.Auditor = auditor
	gw.ServerVersion = version
	gw.AttachControlPlane(db, jobEngine, controlReg)
	// M2-S3: append-only DataService (have/want + PutContents + CommitSnapshot).
	gw.DataService = &agentgw.DataServer{
		Engine: jobEngine, Catalog: db, Keystore: ks, Vaults: vm, Auditor: auditor, Log: log,
	}
	if _, err := gw.Start(*agentAddr); err != nil {
		log.Error("agent gateway", "err", err)
		os.Exit(1)
	}
	defer gw.GracefulStop()

	// HTTPS :8443 — REST + SSE + embedded UI (M2-S5). Auth: dev API token.
	webHandler := web.NewHandler(web.Config{
		DB:       db,
		Auditor:  auditor,
		Events:   eventHub,
		APIToken: apiToken,
		Version:  version,
		Log:      log,
	})

	webSrv := &http.Server{
		Addr:    *webAddr,
		Handler: webHandler,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{serverID.TLSCert},
			MinVersion:   tls.VersionTLS13,
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("web listening (HTTPS)", "addr", *webAddr)
		// Empty cert/key paths: use TLSConfig.Certificates (server identity leaf).
		if err := webSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
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
