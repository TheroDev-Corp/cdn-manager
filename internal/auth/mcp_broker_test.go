package auth

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
)

const brokerTestOrigin = "https://manager.example"

func testBrokerProvider(cfg Config) *Provider {
	p := &Provider{
		publicURL: brokerTestOrigin, mcpIssuer: brokerTestOrigin + MCPBrokerPath, mcpAudience: brokerTestOrigin + MCPPath,
		mcpScopes: []string{service.Read, service.Write}, sessionTTL: time.Hour, secureCookies: true,
		sessions: newExpiringStore[session](cfg.Redis, cfg.RedisPrefix, "session", 25),
	}
	p.broker = newMCPBroker(p, cfg)
	return p
}

func brokerRequestForTest(router http.Handler, method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, brokerTestOrigin+path, strings.NewReader(form.Encode()))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", brokerTestOrigin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func brokerJSONForTest[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	var v T
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func brokerLocation(t *testing.T, response *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	if response.Code != http.StatusSeeOther {
		t.Fatalf("redirect status %d: %s", response.Code, response.Body.String())
	}
	u, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestMCPBrokerAuthorizationAndRefresh(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testMCPBrokerFlow(t, nil, nil) })
	t.Run("redis across replicas", func(t *testing.T) {
		addr := os.Getenv("CDN_MANAGER_TEST_REDIS_URL")
		if addr == "" {
			t.Skip("set CDN_MANAGER_TEST_REDIS_URL to a disposable Redis or Valkey")
		}
		cfg, err := redis.ParseURL(addr)
		if err != nil {
			t.Fatal("invalid fixture Redis URL")
		}
		first, second := redis.NewClient(cfg), redis.NewClient(cfg)
		defer first.Close()
		defer second.Close()
		testMCPBrokerFlow(t, first, second)
	})
}

func testMCPBrokerFlow(t *testing.T, first, second redis.UniversalClient) {
	t.Helper()
	config := Config{Redis: first, RedisPrefix: "test-broker-" + newToken(), MCPBrokerKey: make([]byte, 32)}
	p := testBrokerProvider(config)
	observer := p
	if second != nil {
		config.Redis = second
		observer = testBrokerProvider(config)
	}
	front, back := chi.NewRouter(), chi.NewRouter()
	p.MountMCPBroker(front)
	observer.MountMCPBroker(back)
	metadata := brokerJSONForTest[map[string]any](t, brokerRequestForTest(front, http.MethodGet, "/.well-known/oauth-authorization-server"+MCPBrokerPath, nil, nil), 200)
	if _, advertisesCIMD := metadata["client_id_metadata_document_supported"]; metadata["registration_endpoint"] != p.mcpIssuer+"/register" || advertisesCIMD {
		t.Fatalf("incorrect broker metadata: %+v", metadata)
	}
	if settings := p.MCPConnection(); settings.ClientID != "" || settings.Issuer != p.mcpIssuer {
		t.Fatalf("incorrect connection settings: %+v", settings)
	}

	r := httptest.NewRequest(http.MethodPost, brokerTestOrigin+MCPBrokerPath+"/register", strings.NewReader(`{
		"client_name":"Integration Test", "redirect_uris":["http://127.0.0.1:1010/oauth/callback"],
		"token_endpoint_auth_method":"none", "application_type":"native", "grant_types":["authorization_code","refresh_token"]
	}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	front.ServeHTTP(w, r)
	client := brokerJSONForTest[brokerClient](t, w, 201)
	if client.ID == "" {
		t.Fatal("registration omitted the client ID")
	}
	if _, err := observer.broker.client(client.ID + "A"); err == nil {
		t.Fatal("accepted modified registration")
	}
	verifier := newToken()
	actualRedirect := "http://127.0.0.1:52763/oauth/callback"
	authorize := url.Values{"client_id": {client.ID}, "scope": {"read write"}, "state": {"fixture-state"},
		"response_type": {"code"}, "redirect_uri": {actualRedirect}, "code_challenge_method": {"S256"},
		"code_challenge": {challengeFor(verifier)}, "resource": {p.mcpAudience}}
	wrong := brokerRequestForTest(front, http.MethodGet, MCPBrokerPath+"/authorize?"+strings.Replace(authorize.Encode(), "oauth%2Fcallback", "other", 1), nil, nil)
	brokerJSONForTest[map[string]string](t, wrong, 400)
	location := brokerLocation(t, brokerRequestForTest(front, http.MethodGet, MCPBrokerPath+"/authorize?"+authorize.Encode(), nil, nil))
	if login := brokerLocation(t, brokerRequestForTest(back, http.MethodGet, location.RequestURI(), nil, nil)); login.Path != "/auth/login" || login.Query().Get("mcp_consent") == "" {
		t.Fatalf("expected the web OIDC handoff: %v", login)
	}
	csrf := newToken()
	actor := service.Actor{Subject: "fixture", Name: "Fixture Viewer", Groups: []string{"viewers"}, Client: "web"}
	sessionID, err := p.sessions.issue(t.Context(), session{Actor: actor, CSRF: csrf}, time.Now().Add(time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: p.cookieName("session"), Value: sessionID}
	page := brokerRequestForTest(back, http.MethodGet, location.RequestURI(), nil, cookie)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Integration Test") {
		t.Fatalf("consent did not name the client: %d %s", page.Code, page.Body.String())
	}
	consent := url.Values{"request": {location.Query().Get("request")}, "decision": {"approve"}, "scope": {"read", "delete"}}
	if invalid := brokerRequestForTest(back, http.MethodPost, location.Path, consent, cookie); invalid.Code != 403 {
		t.Fatalf("missing CSRF: %d", invalid.Code)
	}
	consent.Set("csrf", csrf)
	callback := brokerLocation(t, brokerRequestForTest(back, http.MethodPost, location.Path, consent, cookie))
	if callback.Host != "127.0.0.1:52763" || callback.Path != "/oauth/callback" ||
		callback.Query().Get("iss") != p.mcpIssuer || callback.Query().Get("state") != "fixture-state" {
		t.Fatalf("invalid callback: %v", callback)
	}
	form := url.Values{"code": {callback.Query().Get("code")}, "grant_type": {"authorization_code"}, "client_id": {client.ID},
		"redirect_uri": {actualRedirect}, "code_verifier": {verifier}, "resource": {p.mcpAudience}}
	form.Set("resource", brokerTestOrigin+"/other")
	if invalid := brokerJSONForTest[map[string]string](t, brokerRequestForTest(back, http.MethodPost, MCPBrokerPath+"/token", form, nil), 400); invalid["error"] != "invalid_target" {
		t.Fatal(invalid)
	}
	form.Set("resource", p.mcpAudience)
	form.Set("code_verifier", newToken())
	if invalid := brokerJSONForTest[map[string]string](t, brokerRequestForTest(back, http.MethodPost, MCPBrokerPath+"/token", form, nil), 400); invalid["error"] != "invalid_grant" {
		t.Fatal(invalid)
	}
	form.Set("code_verifier", verifier)
	issued := brokerJSONForTest[brokerTokenResponse](t, brokerRequestForTest(back, http.MethodPost, MCPBrokerPath+"/token", form, nil), 200)
	if issued.Scope != "read" || issued.RefreshToken == "" {
		t.Fatalf("scope or refresh mismatch: scope=%q refresh=%v", issued.Scope, issued.RefreshToken != "")
	}
	brokerJSONForTest[map[string]string](t, brokerRequestForTest(back, http.MethodPost, MCPBrokerPath+"/token", form, nil), 400)
	access := httptest.NewRequest(http.MethodPost, brokerTestOrigin+MCPPath, nil)
	access.Header.Set("Authorization", "Bearer "+issued.AccessToken)
	got, err := p.AuthorizeMCP(access)
	if err != nil || got.Actor.Subject != actor.Subject || got.Actor.Client != "mcp" ||
		strings.Join(got.Actor.Scopes, " ") != "read" || strings.Join(got.Actor.Groups, " ") != "viewers" {
		t.Fatalf("invalid access token identity: %+v, %v", got.Actor, err)
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {issued.RefreshToken}, "client_id": {client.ID}}
	refresh.Set("scope", "write")
	if invalid := brokerJSONForTest[map[string]string](t, brokerRequestForTest(front, http.MethodPost, MCPBrokerPath+"/token", refresh, nil), 400); invalid["error"] != "invalid_scope" {
		t.Fatal(invalid)
	}
	refresh.Del("scope")
	rotated := brokerJSONForTest[brokerTokenResponse](t, brokerRequestForTest(front, http.MethodPost, MCPBrokerPath+"/token", refresh, nil), 200)
	if rotated.RefreshToken == "" || rotated.RefreshToken == issued.RefreshToken {
		t.Fatal("refresh token did not rotate")
	}
	brokerJSONForTest[map[string]string](t, brokerRequestForTest(back, http.MethodPost, MCPBrokerPath+"/token", refresh, nil), 400)
	access.Header.Set("Authorization", "Bearer "+rotated.AccessToken)
	if _, err := observer.AuthorizeMCP(access); err == nil {
		t.Fatal("refresh replay did not revoke the grant")
	}
}

func TestMCPBrokerRejectsClientMetadataURLs(t *testing.T) {
	p := testBrokerProvider(Config{})
	const id = "https://client.example/oauth/mcp.json"
	if _, err := p.broker.client(id); err == nil {
		t.Fatal("broker accepted a client without dynamic registration")
	}
	sessionID, err := p.sessions.issue(t.Context(), session{Actor: service.Actor{Subject: "legacy"}}, time.Now().Add(time.Hour), "", "")
	if err != nil {
		t.Fatal(err)
	}
	grant := brokerGrant{ClientID: id, Session: sha256.Sum256([]byte(sessionID)), Resource: p.mcpAudience, Expires: time.Now().Add(time.Hour)}
	grantID, err := p.broker.grants.issue(t.Context(), grant, grant.Expires, "", "")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := p.broker.issueTokens(t.Context(), grantID, grant, []string{"read"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.broker.accessToken(t.Context(), tokens.AccessToken); err == nil {
		t.Fatal("broker accepted a previously issued CIMD access token")
	}
	if _, err := p.broker.rotate(t.Context(), tokens.RefreshToken, id, ""); err == nil {
		t.Fatal("broker refreshed a previously issued CIMD token")
	}
}
