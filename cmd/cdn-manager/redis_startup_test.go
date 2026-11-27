package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TheroDev-Corp/cdn-manager/internal/telemetry"
)

func TestRedisUnavailableKeepsManagerAliveAndUnready(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CDN_MANAGER_REDIS_URL", "redis://"+address)
	client, err := openRedis()
	if err != nil {
		t.Fatalf("initializing Redis should not require a connection: %v", err)
	}
	defer client.Close()
	probes := internalRoutes(nil, telemetry.New(), client)
	for _, check := range []struct {
		path   string
		status int
	}{{"/livez", http.StatusOK}, {"/readyz", http.StatusServiceUnavailable}, {"/livez", http.StatusOK}} {
		response := httptest.NewRecorder()
		probes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, check.path, nil))
		if response.Code != check.status {
			t.Errorf("%s = %d, want %d", check.path, response.Code, check.status)
		}
	}
}
