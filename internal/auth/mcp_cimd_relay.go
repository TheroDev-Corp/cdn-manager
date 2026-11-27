package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const cimdRelayPath = "/v1/client-metadata"

type cimdRelayClient struct {
	endpoint string
	token    string
	http     *http.Client
}

func validCIMDRelayToken(token string) bool {
	key, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(key) == 32
}

func newCIMDRelayClient(endpoint, token string) (*cimdRelayClient, error) {
	if endpoint == "" && token == "" {
		return nil, nil
	}
	if !validCIMDRelayToken(token) {
		return nil, errors.New("MCP metadata relay token must contain 32 base64url-encoded random bytes")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != cimdRelayPath || u.RawQuery != "" || u.ForceQuery || strings.Contains(endpoint, "#") {
		return nil, errors.New("invalid MCP metadata relay endpoint")
	}
	if u.Scheme != "https" {
		ip, ipErr := netip.ParseAddr(u.Hostname())
		// Plain HTTP is only supported over a private, encrypted overlay
		// address or for local verification on loopback.
		if u.Scheme != "http" || ipErr != nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
			return nil, errors.New("MCP metadata relay requires HTTPS or a private IP endpoint")
		}
	}
	return &cimdRelayClient{
		endpoint: u.String(), token: token,
		http: &http.Client{Timeout: 8 * time.Second,
			Transport: &http.Transport{
				DialContext:            (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
				TLSHandshakeTimeout:    3 * time.Second,
				MaxResponseHeaderBytes: 16 << 10,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (relay *cimdRelayClient) fetch(ctx context.Context, id string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, relay.endpoint, strings.NewReader(id))
	if err != nil {
		return nil, errors.New("invalid metadata relay request")
	}
	req.Header.Set("Authorization", "Bearer "+relay.token)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := relay.http.Do(req)
	if err != nil {
		return nil, metadataTransportError(err)
	}
	defer resp.Body.Close()
	return readClientMetadata(resp)
}

// NewMCPMetadataRelay serves public CIMD documents on a trusted egress network.
// The same metadata validation applies at the relay and the consuming broker.
func NewMCPMetadataRelay(token string, logger *slog.Logger) (http.Handler, error) {
	if !validCIMDRelayToken(token) {
		return nil, errors.New("MCP metadata relay token must contain 32 base64url-encoded random bytes")
	}
	return cimdRelayHandler(token, logger, publicMetadataClient()), nil
}

func cimdRelayHandler(token string, logger *slog.Logger, client *http.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST "+cimdRelayPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
		if err != nil || !validMetadataURL(string(raw)) {
			http.Error(w, "invalid metadata URL", http.StatusBadRequest)
			return
		}
		id := string(raw)
		payload, err := fetchClientMetadata(r.Context(), client, id)
		if err == nil {
			_, err = decodeClientMetadata(payload, id)
		}
		if err != nil {
			logger.WarnContext(r.Context(), "MCP client metadata unavailable", "reason", err.Error())
			http.Error(w, "metadata unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	})
	return mux
}
