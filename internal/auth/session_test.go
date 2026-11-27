package auth

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
)

type tokenTransport func(*http.Request) (*http.Response, error)

func (send tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) { return send(r) }

func TestWebSessionSurvivesIDTokenExpiry(t *testing.T) {
	checkWebSessionExpiry(t, nil, nil)
}

func TestRedisWebSessionSurvivesIDTokenExpiryAcrossReplicas(t *testing.T) {
	addr := os.Getenv("CDN_MANAGER_TEST_REDIS_URL")
	if addr == "" {
		t.Skip("set CDN_MANAGER_TEST_REDIS_URL to a disposable Redis or Valkey")
	}
	options, err := redis.ParseURL(addr)
	if err != nil {
		t.Fatal("invalid fixture Redis URL")
	}
	first, second := redis.NewClient(options), redis.NewClient(options)
	defer first.Close()
	defer second.Close()
	checkWebSessionExpiry(t, first, second)
}

func checkWebSessionExpiry(t *testing.T, first, second redis.UniversalClient) {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://issuer.example"
	const clientID = "web"
	const ttl = 8 * time.Hour
	prefix := "test-session-" + newToken()
	var nonce string
	var expires time.Time
	client := &http.Client{Transport: tokenTransport(func(r *http.Request) (*http.Response, error) {
		expires = time.Now().Truncate(time.Second).Add(3 * time.Second)
		claims, err := json.Marshal(map[string]any{
			"iss": issuer, "aud": clientID, "sub": "owner", "nonce": nonce,
			"iat": expires.Add(-5 * time.Minute).Unix(), "exp": expires.Unix(),
		})
		if err != nil {
			return nil, err
		}
		encode := base64.RawURLEncoding.EncodeToString
		unsigned := encode([]byte(`{"alg":"EdDSA"}`)) + "." + encode(claims)
		token := unsigned + "." + encode(ed25519.Sign(key, []byte(unsigned)))
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "fixture", "id_token": token})
		return w.Result(), nil
	})}
	p := &Provider{
		sessionTTL: ttl, httpClient: client,
		oauth: oauth2.Config{ClientID: clientID, Endpoint: oauth2.Endpoint{
			AuthURL: issuer + "/authorize", TokenURL: issuer + "/token", AuthStyle: oauth2.AuthStyleInHeader,
		}},
		webVerifier: oidc.NewVerifier(issuer, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{public}}, &oidc.Config{
			ClientID: clientID, SupportedSigningAlgs: []string{"EdDSA"},
		}),
		logins:   newExpiringStore[login](first, prefix, "login", 10),
		sessions: newExpiringStore[session](first, prefix, "session", 10),
	}
	loginResponse := httptest.NewRecorder()
	p.Login(loginResponse, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	redirect, err := url.Parse(loginResponse.Header().Get("Location"))
	if err != nil || loginResponse.Code != http.StatusSeeOther {
		t.Fatalf("login response: %d, %v", loginResponse.Code, err)
	}
	nonce = redirect.Query().Get("nonce")
	callback := httptest.NewRequest(http.MethodGet, "/auth/callback?code=fixture&state="+redirect.Query().Get("state"), nil)
	callback.AddCookie(loginResponse.Result().Cookies()[0])
	response := httptest.NewRecorder()
	p.Callback(response, callback)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", response.Code, response.Body.String())
	}
	var cookie *http.Cookie
	for _, value := range response.Result().Cookies() {
		if value.Name == p.cookieName("session") {
			cookie = value
		}
	}
	if cookie == nil {
		t.Fatal("missing session cookie")
	}
	observer := p
	if second != nil {
		observer = &Provider{sessions: newExpiringStore[session](second, prefix, "session", 10)}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	request.AddCookie(cookie)
	if _, _, err := p.Web(request); err != nil {
		t.Fatalf("fresh session: %v", err)
	}
	time.Sleep(time.Until(expires))
	actor, csrf, err := observer.Web(request)
	if err != nil || actor.Subject != "owner" || cookie.MaxAge != int(ttl.Seconds()) {
		t.Fatalf("after ID token expiration: subject=%q cookie Max-Age=%ds err=%v", actor.Subject, cookie.MaxAge, err)
	}
	logout := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	logout.AddCookie(cookie)
	logout.Header.Set("X-CSRF-Token", csrf)
	loggedOut := httptest.NewRecorder()
	observer.Logout(loggedOut, logout)
	if _, _, err = p.Web(request); loggedOut.Code != http.StatusNoContent || err != ErrUnauthorized {
		t.Fatalf("after logout: status=%d err=%v", loggedOut.Code, err)
	}
}
