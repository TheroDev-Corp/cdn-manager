package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/api"
	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/mcp"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
	"github.com/TheroDev-Corp/cdn-manager/internal/telemetry"
	"github.com/TheroDev-Corp/cdn-manager/web"
	"github.com/go-chi/chi/v5"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, logger); err != nil {
		logger.Error("manager stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := readConfig(setting("CDN_MANAGER_CONFIG", "config.json"))
	if err != nil {
		return err
	}
	sources, err := cfg.storage()
	if err != nil {
		return err
	}
	authSettings, err := cfg.Auth.provider()
	if err != nil {
		return err
	}
	stateDir := setting("CDN_MANAGER_STATE_DIR", ".cdn-manager")
	lock, err := lockState(stateDir)
	if err != nil {
		return err
	}
	defer lock.Close()

	metrics := telemetry.New()
	for i := range sources {
		sources[i].Observe = metrics.ObserveS3
	}
	store, err := storage.New(sources, stateDir)
	if err != nil {
		return err
	}
	provider, err := auth.New(ctx, authSettings)
	if err != nil {
		return err
	}
	backend := service.New(store, cfg.Sources, logger)
	backend.Metrics = metrics

	public := &http.Server{
		Addr: setting("CDN_MANAGER_LISTEN", ":8080"), Handler: publicRoutes(backend, provider, metrics),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 75 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	internal := &http.Server{
		Addr: setting("CDN_MANAGER_INTERNAL_LISTEN", ":9090"), Handler: internalRoutes(backend, metrics),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second,
	}
	publicListener, err := net.Listen("tcp", public.Addr)
	if err != nil {
		return fmt.Errorf("listen for web and MCP: %w", err)
	}
	defer publicListener.Close()
	internalListener, err := net.Listen("tcp", internal.Addr)
	if err != nil {
		return fmt.Errorf("listen for internal probes and metrics: %w", err)
	}
	defer internalListener.Close()
	errs := make(chan error, 2)
	go func() { errs <- public.Serve(publicListener) }()
	go func() { errs <- internal.Serve(internalListener) }()
	logger.Info("manager listening", "public", publicListener.Addr(), "internal", internalListener.Addr())

	workers, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		repeat(workers, 30*time.Second, backend.RefreshHealth)
	})
	wg.Go(func() {
		repeat(workers, 15*time.Minute, func(workerCtx context.Context) {
			sweepCtx, stop := context.WithTimeout(workerCtx, time.Minute)
			defer stop()
			if err := store.Sweep(sweepCtx); err != nil && sweepCtx.Err() == nil {
				logger.Warn("expired uploads could not all be cleaned up")
			}
		})
	})
	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
	defer stop()
	if shutdownErr := public.Shutdown(shutdown); shutdownErr != nil {
		_ = public.Close()
		err = errors.Join(err, shutdownErr)
	}
	if shutdownErr := internal.Shutdown(shutdown); shutdownErr != nil {
		_ = internal.Close()
		err = errors.Join(err, shutdownErr)
	}
	wg.Wait()
	return err
}

func lockState(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, ".manager.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("open state directory lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("acquire state directory lock: %w", err)
	}
	return file, nil
}

func repeat(ctx context.Context, every time.Duration, run func(context.Context)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for ctx.Err() == nil {
		run(ctx)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func publicRoutes(backend *service.Backend, provider *auth.Provider, metrics *telemetry.Metrics) http.Handler {
	router := chi.NewRouter()
	router.Use(metrics.ObserveHTTP)
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "strict-origin")
			w.Header().Set("X-Frame-Options", "DENY")
			next.ServeHTTP(w, r)
		})
	})
	router.Get("/auth/login", provider.Login)
	router.Get("/auth/callback", provider.Callback)
	router.Post("/auth/logout", provider.Logout)
	router.Get(auth.MetadataPath, provider.Metadata)
	mcpServer := mcp.New(backend, provider)
	router.Handle(auth.MCPPath, mcpServer)
	router.Put(mcp.TransferPath, mcpServer.Transfer)
	api.Mount(router, backend, provider)

	assets := web.Assets()
	files := http.FileServerFS(assets)
	router.Get("/", files.ServeHTTP)
	router.Head("/", files.ServeHTTP)
	serveAsset := func(w http.ResponseWriter, r *http.Request) {
		if info, err := fs.Stat(assets, r.URL.Path[1:]); err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	}
	router.Get("/assets/*", serveAsset)
	router.Head("/assets/*", serveAsset)
	return router
}

func internalRoutes(backend *service.Backend, metrics *telemetry.Metrics) http.Handler {
	router := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") }
	router.HandleFunc("GET /livez", ok)
	router.HandleFunc("GET /readyz", ok)
	router.Handle("GET /metrics", metrics.Handler())
	router.HandleFunc("GET /sources/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(backend.Health())
	})
	return router
}
