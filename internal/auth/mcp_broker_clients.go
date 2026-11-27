package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
)

const registeredClientPrefix = "cdnmcp_client_"

type brokerClient struct {
	ID          string   `json:"client_id,omitempty"`
	Name        string   `json:"client_name"`
	Redirects   []string `json:"redirect_uris"`
	AppType     string   `json:"application_type,omitempty"`
	AuthMethod  string   `json:"token_endpoint_auth_method"`
	AuthMethods []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	Grants      []string `json:"grant_types,omitempty"`
	Responses   []string `json:"response_types,omitempty"`
}

func (c brokerClient) canRefresh() bool { return slices.Contains(c.Grants, "refresh_token") }

func (b *mcpBroker) register(w http.ResponseWriter, r *http.Request) {
	var client brokerClient
	mimeType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, brokerBodyLimit))
	if mimeType != "application/json" || decoder.Decode(&client) != nil || decoder.Decode(new(any)) != io.EOF {
		brokerError(w, http.StatusBadRequest, "invalid_client_metadata")
		return
	}
	if code := client.validate(); code != "" {
		brokerError(w, http.StatusBadRequest, code)
		return
	}
	client.ID = b.sealClient(client)
	brokerJSON(w, http.StatusCreated, client)
}

func (c *brokerClient) validate() string {
	if c.ID != "" {
		return "invalid_client_metadata"
	}
	if c.Name == "" {
		c.Name = "MCP client"
	}
	if c.AuthMethod == "" {
		c.AuthMethod = "none"
	}
	if len(c.AuthMethods) != 0 {
		return "invalid_client_metadata"
	}
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 120 || strings.ContainsFunc(c.Name, unicode.IsControl) ||
		c.AuthMethod != "none" || (c.AppType != "" && c.AppType != "native" && c.AppType != "web") {
		return "invalid_client_metadata"
	}
	if len(c.Redirects) == 0 || len(c.Redirects) > 8 {
		return "invalid_redirect_uri"
	}
	for _, raw := range c.Redirects {
		if u := brokerRedirect(raw); u == nil || (c.AppType == "web" && u.Scheme == "http") {
			return "invalid_redirect_uri"
		}
	}
	if len(c.Grants) == 0 {
		c.Grants = []string{"authorization_code"}
	}
	if len(c.Responses) == 0 {
		c.Responses = []string{"code"}
	}
	if len(c.Grants) > 2 || !slices.Contains(c.Grants, "authorization_code") {
		return "invalid_client_metadata"
	}
	for _, grant := range c.Grants {
		if grant != "authorization_code" && grant != "refresh_token" {
			return "invalid_client_metadata"
		}
	}
	if len(c.Responses) != 1 || c.Responses[0] != "code" {
		return "invalid_client_metadata"
	}
	return ""
}

func (b *mcpBroker) sealClient(c brokerClient) string {
	payload, _ := json.Marshal(c)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return registeredClientPrefix + encoded + "." + base64.RawURLEncoding.EncodeToString(b.clientSignature(encoded))
}

func (b *mcpBroker) clientSignature(encoded string) []byte {
	mac := hmac.New(sha256.New, b.key)
	_, _ = io.WriteString(mac, "cdn-manager MCP OAuth public client v1\x00"+b.provider.mcpIssuer+"\x00"+encoded)
	return mac.Sum(nil)
}

func (b *mcpBroker) client(id string) (brokerClient, error) {
	var client brokerClient
	if len(id) > 8500 {
		return client, ErrUnauthorized
	}
	registered, ok := strings.CutPrefix(id, registeredClientPrefix)
	if !ok {
		return client, ErrUnauthorized
	}
	encoded, signature, ok := strings.Cut(registered, ".")
	if !ok || len(signature) != 43 {
		return client, ErrUnauthorized
	}
	mac, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(mac, b.clientSignature(encoded)) {
		return client, ErrUnauthorized
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || json.Unmarshal(payload, &client) != nil || client.validate() != "" {
		return client, ErrUnauthorized
	}
	return client, nil
}

func brokerRedirect(raw string) *url.URL {
	if len(raw) > 512 || strings.ContainsAny(raw, "#\\") || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Hostname() == "" || strings.ContainsAny(u.Host, "%;<>\"'") {
		return nil
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil
	}
	for _, key := range []string{"code", "error", "error_description", "error_uri", "state", "iss"} {
		if q.Has(key) {
			return nil
		}
	}
	if u.Scheme == "https" || (u.Scheme == "http" && brokerLoopback(u)) {
		return u
	}
	return nil
}

func brokerLoopback(u *url.URL) bool {
	return strings.EqualFold(u.Hostname(), "localhost") || net.ParseIP(u.Hostname()).IsLoopback()
}

func (c brokerClient) acceptsRedirect(raw string) bool {
	target := brokerRedirect(raw)
	if target == nil {
		return false
	}
	for _, registered := range c.Redirects {
		if registered == raw {
			return true
		}
		if c.AppType != "web" && target.Scheme == "http" && brokerLoopback(target) {
			candidate, _ := url.Parse(registered)
			if candidate.Scheme == "http" && candidate.Hostname() == target.Hostname() &&
				candidate.EscapedPath() == target.EscapedPath() && candidate.RawQuery == target.RawQuery && candidate.ForceQuery == target.ForceQuery {
				return true
			}
		}
	}
	return false
}
