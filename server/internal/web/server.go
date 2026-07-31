package web

import (
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/retention"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// Config wires the HTTPS :8443 handler tree.
type Config struct {
	DB        *catalog.DB
	Auditor   *audit.Writer
	Events    *scheduler.EventHub
	Engine    *scheduler.Engine
	Vaults    *vault.Manager
	Keystore  *keystore.Store
	Retention *retention.Service
	// EnableDestructiveAPI opts in M5 forget/prune/retention/scrub REST
	// (default false until M6 sessions — M5-F1).
	EnableDestructiveAPI bool
	// APIToken is the dev local token (from LoadOrCreateAPIToken).
	APIToken string
	Version  string
	Log      *slog.Logger
}

// NewHandler builds the root http.Handler for :8443.
// Open: /healthz, /version. Gated: /api/v1/*. Static UI: / and SPA fallback.
func NewHandler(cfg Config) http.Handler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if cfg.DB != nil {
			if err := cfg.DB.Ping(r.Context()); err != nil {
				http.Error(w, "catalog unhealthy", http.StatusServiceUnavailable)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		v := cfg.Version
		if v == "" {
			v = "dev"
		}
		_, _ = w.Write([]byte(v + "\n"))
	})

	api := &API{
		DB:                   cfg.DB,
		Auditor:              cfg.Auditor,
		Events:               cfg.Events,
		Engine:               cfg.Engine,
		Vaults:               cfg.Vaults,
		Keystore:             cfg.Keystore,
		Retention:            cfg.Retention,
		EnableDestructiveAPI: cfg.EnableDestructiveAPI,
		Version:              cfg.Version,
		Log:                  cfg.Log,
	}
	api.Mount(mux, cfg.APIToken)

	// Embedded UI (SPA).
	static := UIFileSystem()
	fileServer := http.FileServer(http.FS(static))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Never serve UI for API paths (auth must have already matched).
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		// Try exact file; SPA fallback to index.html for client routes.
		if f, err := static.Open(path); err == nil {
			_ = f.Close()
			// Cache hashed assets aggressively; index.html short cache.
			if path == "index.html" {
				w.Header().Set("Cache-Control", "no-cache")
			} else if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/index.html"
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r2)
	})

	return withRequestLog(cfg.Log, mux)
}

func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		// Do not log Authorization / tokens.
		log.Debug("http",
			"method", r.Method,
			"path", r.URL.Path,
			"dur", time.Since(start).String(),
		)
	})
}

// UIFileSystem returns the embedded dist FS rooted at dist/.
func UIFileSystem() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Should never happen with committed placeholder.
		return distFS
	}
	return sub
}
