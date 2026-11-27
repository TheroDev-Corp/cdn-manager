package auth

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCIMDRelayKeepsClientValidationAndRequiresAuthentication(t *testing.T) {
	const clientID = "https://client.example/client.json"
	const metadata = `{"client_id":"https://client.example/client.json","client_name":"Browser client","application_type":"native","redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none"}`
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var fetches atomic.Int32
	upstream := &http.Client{Transport: tokenTransport(func(r *http.Request) (*http.Response, error) {
		fetches.Add(1)
		if r.Method != http.MethodGet || r.URL.String() != clientID {
			t.Errorf("unexpected metadata request %s %s", r.Method, r.URL)
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(metadata)),
		}, nil
	})}
	relay := httptest.NewServer(cimdRelayHandler(token, logger, upstream))
	defer relay.Close()

	req, _ := http.NewRequest(http.MethodPost, relay.URL+cimdRelayPath, strings.NewReader(clientID))
	res, err := relay.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || fetches.Load() != 0 {
		t.Fatalf("unauthenticated relay: HTTP %d, %d fetches", res.StatusCode, fetches.Load())
	}

	relayClient, err := newCIMDRelayClient(relay.URL+cimdRelayPath, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relayClient.fetch(context.Background(), "https://127.0.0.1/client.json"); err == nil || fetches.Load() != 0 {
		t.Fatal("relay must reject a non-public client ID before requesting it")
	}

	blocked := &http.Client{Transport: tokenTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody}, nil
	})}
	broker := &mcpBroker{metadataClient: blocked, metadataRelay: relayClient}
	client, err := broker.client(context.Background(), clientID)
	if err != nil || fetches.Load() != 1 {
		t.Fatalf("relay fallback: client %+v, %d fetches, error %v", client, fetches.Load(), err)
	}
	if !client.acceptsRedirect("http://127.0.0.1:45877/callback") || client.acceptsRedirect("http://127.0.0.1:45877/other") {
		t.Fatal("relay must preserve exact callback matching apart from native loopback port")
	}
}
