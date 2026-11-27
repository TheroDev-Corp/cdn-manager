package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
)

func serveMCPMetadata(ctx context.Context, logger *slog.Logger) error {
	handler, err := auth.NewMCPMetadataRelay(os.Getenv("CDN_MANAGER_METADATA_RELAY_TOKEN"), logger)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", setting("CDN_MANAGER_METADATA_RELAY_LISTEN", "127.0.0.1:9097"))
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	logger.Info("MCP metadata relay listening", "address", listener.Addr())
	select {
	case err := <-stopped:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
