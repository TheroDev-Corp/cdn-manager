package mcp

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type issuer struct {
	server *httptest.Server
	key    ed25519.PrivateKey
	mu     sync.RWMutex
	nonce  string
	pkce   string
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	i := &issuer{key: private}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": i.server.URL, "authorization_endpoint": i.server.URL + "/authorize",
			"token_endpoint": i.server.URL + "/token", "jwks_uri": i.server.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"EdDSA"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": "fixture",
			"x": base64.RawURLEncoding.EncodeToString(public),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		client, secret, _ := r.BasicAuth()
		pkce := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		i.mu.RLock()
		nonce, challenge := i.nonce, i.pkce
		i.mu.RUnlock()
		if client != "web" || secret != "fixture-secret" || r.FormValue("code") != "browser-code" ||
			base64.RawURLEncoding.EncodeToString(pkce[:]) != challenge {
			http.Error(w, "bad code exchange", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "unused", "token_type": "Bearer",
			"id_token": i.issue("JWT", map[string]any{"aud": "web", "nonce": nonce}),
		})
	})
	i.server = httptest.NewServer(mux)
	t.Cleanup(i.server.Close)
	return i
}

func (i *issuer) issue(kind string, custom map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "fixture", "typ": kind})
	claims := map[string]any{
		"iss": i.server.URL, "sub": "alice", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		"name": "Alice", "groups": []string{"editors"},
	}
	for key, value := range custom {
		claims[key] = value
	}
	payload, _ := json.Marshal(claims)
	encode := base64.RawURLEncoding.EncodeToString
	input := encode(header) + "." + encode(payload)
	return input + "." + encode(ed25519.Sign(i.key, []byte(input)))
}

func (i *issuer) provider(t *testing.T, publicURL string) *auth.Provider {
	t.Helper()
	provider, err := auth.New(t.Context(), i.providerConfig(publicURL))
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func (i *issuer) providerConfig(publicURL string) auth.Config {
	return auth.Config{
		PublicURL: publicURL, OIDCIssuer: i.server.URL, OIDCClientID: "web", OIDCClientSecret: "fixture-secret",
		MCPAudience: "cdn-mcp", AllowInsecureLocalhost: true,
	}
}

func (i *issuer) expectLogin(query url.Values) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.nonce = query.Get("nonce")
	i.pkce = query.Get("code_challenge")
}

func TestBrowserLoginAndLogout(t *testing.T) {
	i := newIssuer(t)
	p := i.provider(t, "http://127.0.0.1:8080")
	login := httptest.NewRecorder()
	p.Login(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	location, err := login.Result().Location()
	if err != nil {
		t.Fatal(err)
	}
	query := location.Query()
	if login.Code != http.StatusSeeOther || query.Get("code_challenge_method") != "S256" || query.Get("state") == "" || query.Get("nonce") == "" {
		t.Fatalf("missing PKCE or transaction parameters: %d %s", login.Code, location)
	}
	i.expectLogin(query)
	callback := func(state string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/auth/callback?code=browser-code&state="+url.QueryEscape(state), nil)
		r.AddCookie(login.Result().Cookies()[0])
		w := httptest.NewRecorder()
		p.Callback(w, r)
		return w
	}
	if w := callback("wrong-state"); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong state: %d", w.Code)
	}
	correct := callback(query.Get("state"))
	if correct.Code != http.StatusSeeOther {
		t.Fatalf("valid callback: %d %s", correct.Code, correct.Body)
	}
	cookies := correct.Result().Cookies()
	if len(cookies) != 2 || !cookies[1].HttpOnly || cookies[1].SameSite != http.SameSiteLaxMode || cookies[1].MaxAge <= 0 {
		t.Fatalf("unexpected session cookie attributes: %#v", cookies)
	}
	if w := callback(query.Get("state")); w.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	r.AddCookie(cookies[1])
	actor, csrf, err := p.Web(r)
	if err != nil || actor.Subject != "alice" || csrf == "" || len(actor.Groups) != 1 || actor.Groups[0] != "editors" {
		t.Fatalf("web identity: %v %v", actor, err)
	}
	logout := httptest.NewRecorder()
	p.Logout(logout, r)
	if logout.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF token: %d", logout.Code)
	}
	r.Header.Set("X-CSRF-Token", csrf)
	r.Header.Set("Origin", "https://outside.example.test")
	logout = httptest.NewRecorder()
	p.Logout(logout, r)
	if logout.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout: %d", logout.Code)
	}
	r.Header.Set("Origin", p.PublicURL())
	logout = httptest.NewRecorder()
	p.Logout(logout, r)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("valid logout: %d", logout.Code)
	}
	if _, _, err := p.Web(r); err == nil {
		t.Fatal("logged-out session remains valid")
	}
	login = httptest.NewRecorder()
	p.Login(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	location, err = login.Result().Location()
	if err != nil {
		t.Fatal(err)
	}
	query = location.Query()
	i.pkce = query.Get("code_challenge")
	i.nonce = "wrong-nonce"
	if w := callback(query.Get("state")); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong nonce: %d", w.Code)
	}
}

type doRequest func(*http.Request) (*http.Response, error)

func (do doRequest) RoundTrip(r *http.Request) (*http.Response, error) { return do(r) }

func TestMCPAuthorizationAndSDK(t *testing.T) {
	i := newIssuer(t)
	backend := service.New(nil, []service.SourceConfig{{Roles: service.Roles{Editor: []string{"editors"}}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	manager := httptest.NewServer(mux)
	t.Cleanup(manager.Close)
	p := i.provider(t, manager.URL)
	mcpServer := New(backend, p)
	mux.Handle("/mcp", mcpServer)
	mux.HandleFunc("GET "+auth.MetadataPath, p.Metadata)
	mux.HandleFunc("PUT "+TransferPath, mcpServer.Transfer)
	good := i.issue("at+jwt", map[string]any{"aud": "cdn-mcp", "scope": "read"})
	for _, test := range []struct {
		name, token, origin string
		status              int
	}{
		{name: "no bearer token", status: 401},
		{name: "wrong issuer", token: i.issue("at+jwt", map[string]any{"iss": "https://wrong.example", "aud": "cdn-mcp", "scope": "read"}), status: 401},
		{name: "wrong audience", token: i.issue("at+jwt", map[string]any{"aud": "wrong", "scope": "read"}), status: 401},
		{name: "expired", token: i.issue("at+jwt", map[string]any{"aud": "cdn-mcp", "scope": "read", "exp": time.Now().Add(-time.Hour).Unix()}), status: 401},
		{name: "ID token", token: i.issue("JWT", map[string]any{"aud": "cdn-mcp", "scope": "read"}), status: 401},
		{name: "no permitted scope", token: i.issue("at+jwt", map[string]any{"aud": "cdn-mcp", "scope": "openid"}), status: 403},
		{name: "cross origin", token: good, origin: "https://wrong.example", status: 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if test.token != "" {
				r.Header.Set("Authorization", "Bearer "+test.token)
			}
			if test.origin != "" {
				r.Header.Set("Origin", test.origin)
			}
			w := httptest.NewRecorder()
			mcpServer.ServeHTTP(w, r)
			if w.Code != test.status || (w.Code == 401 && !strings.Contains(w.Header().Get("WWW-Authenticate"), manager.URL+auth.MetadataPath)) {
				t.Fatalf("status %d, challenge %q", w.Code, w.Header().Get("WWW-Authenticate"))
			}
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "acceptance", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: manager.URL + "/mcp", DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: doRequest(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+good)
			return http.DefaultTransport.RoundTrip(r)
		})},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "list_sources", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("SDK call failed: %+v %v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var sources sourcesResult
	if err := json.Unmarshal(raw, &sources); err != nil || len(sources.Sources) != 1 || sources.Sources[0].Capabilities["upload"] {
		t.Fatalf("intersected capabilities: %s, %v", raw, err)
	}
	if result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "start_upload", Arguments: map[string]any{
		"source": "hidden", "bucket": "hidden", "key": "file", "size": 1,
	}}); err == nil && !result.IsError {
		t.Fatalf("write succeeded with read scope: %+v", result)
	}
	challengeRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	challengeRequest.Header.Set("Authorization", "Bearer "+good)
	challengeRequest.Header.Set("Mcp-Method", "tools/call")
	challengeRequest.Header.Set("Mcp-Name", "start_upload")
	challenge := httptest.NewRecorder()
	mcpServer.ServeHTTP(challenge, challengeRequest)
	if challenge.Code != http.StatusForbidden || !strings.Contains(challenge.Header().Get("WWW-Authenticate"), `error="insufficient_scope", scope="write"`) {
		t.Fatalf("missing write step-up challenge: %d %s", challenge.Code, challenge.Header().Get("WWW-Authenticate"))
	}

	// Authentik's default access JWT has azp + uid and uses generic JWT typ.
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+i.issue("JWT", map[string]any{
		"aud": "cdn-mcp", "azp": "cdn-mcp", "uid": "fixture", "scope": "read write",
	}))
	token, err := p.AuthorizeMCP(request)
	if err != nil {
		t.Fatalf("Authentik-style access token: %v", err)
	}
	expiry := time.Now().Add(time.Minute)
	grant, until, err := p.IssueUploadGrant(t.Context(), token, "upload", 2, 123, expiry)
	if err != nil || !until.Equal(expiry) {
		t.Fatalf("grant expiration %v, expected %v", until, expiry)
	}
	request.Header.Set("Authorization", "Bearer "+grant)
	scoped, err := p.UploadGrant(request)
	if err != nil || scoped.UploadID != "upload" || scoped.Number != 2 || scoped.Size != 123 || scoped.Actor.Subject != "alice" || len(scoped.Actor.Scopes) != 1 {
		t.Fatalf("wrong grant scope: %+v, %v", scoped, err)
	}
	if _, err := p.AuthorizeMCP(request); err == nil {
		t.Fatal("transfer grant accepted as MCP access token")
	}
	transfer := httptest.NewRequest(http.MethodPut, TransferPath, strings.NewReader("wrong size"))
	transfer.Header.Set("Authorization", "Bearer "+grant)
	transferResponse := httptest.NewRecorder()
	mcpServer.Transfer(transferResponse, transfer)
	if transferResponse.Code != http.StatusBadRequest {
		t.Fatalf("mismatched part length: %d", transferResponse.Code)
	}
	transfer.Header.Set("Authorization", "Bearer "+good)
	transferResponse = httptest.NewRecorder()
	mcpServer.Transfer(transferResponse, transfer)
	if transferResponse.Code != http.StatusUnauthorized {
		t.Fatalf("OAuth token accepted as transfer grant: %d", transferResponse.Code)
	}
	if _, _, err := p.IssueUploadGrant(t.Context(), token, "upload", 2, 123, time.Now().Add(-time.Second)); err == nil {
		t.Fatal("issued a grant for an expired upload session")
	}
}
