package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/api"
	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
)

func TestRedisAuthAcrossReplicas(t *testing.T) {
	addr := os.Getenv("CDN_MANAGER_TEST_REDIS_URL")
	if addr == "" {
		t.Skip("set CDN_MANAGER_TEST_REDIS_URL to a disposable Redis or Valkey")
	}
	options, err := redis.ParseURL(addr)
	if err != nil {
		t.Fatal("invalid fixture Redis URL")
	}
	options.MaxRetries = -1
	clients := []*redis.Client{redis.NewClient(options), redis.NewClient(options)}
	prefix := "cdn-manager-test-auth:" + time.Now().Format("20060102150405.000000000")
	t.Cleanup(func() {
		for _, client := range clients {
			_ = client.Close()
		}
	})
	for _, client := range clients {
		if err := client.Ping(t.Context()).Err(); err != nil {
			t.Fatalf("Redis fixture unavailable: %v", err)
		}
	}

	i := newIssuer(t)
	const origin = "http://127.0.0.1:8080"
	newReplica := func(client *redis.Client) (*auth.Provider, *httptest.Server) {
		cfg := i.providerConfig(origin)
		cfg.Redis = client
		cfg.RedisPrefix = prefix
		provider, err := auth.New(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		router := chi.NewRouter()
		router.Get("/auth/login", provider.Login)
		router.Get("/auth/callback", provider.Callback)
		router.Post("/auth/logout", provider.Logout)
		transfer := &Server{provider: provider}
		router.Put(TransferPath, transfer.Transfer)
		api.Mount(router, nil, provider)
		server := httptest.NewServer(router)
		t.Cleanup(server.Close)
		return provider, server
	}
	first, one := newReplica(clients[0])
	second, two := newReplica(clients[1])
	browser := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request := func(method, target string, cookie *http.Cookie, csrf string) *http.Request {
		req, err := http.NewRequestWithContext(t.Context(), method, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		req.Header.Set("Origin", origin)
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		return req
	}
	send := func(req *http.Request, expected int) *http.Response {
		t.Helper()
		response, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		if response.StatusCode != expected {
			t.Fatalf("%s %s returned %d, want %d", req.Method, req.URL.Path, response.StatusCode, expected)
		}
		return response
	}
	login := send(request(http.MethodGet, one.URL+"/auth/login", nil, ""), http.StatusSeeOther)
	location, err := login.Location()
	if err != nil {
		t.Fatal(err)
	}
	i.expectLogin(location.Query())
	loginCookie := login.Cookies()[0]
	two.Close()
	second, two = newReplica(clients[1])
	callback := func(server, state string) *http.Request {
		return request(http.MethodGet, server+"/auth/callback?code=browser-code&state="+url.QueryEscape(state), loginCookie, "")
	}
	send(callback(two.URL, strings.Repeat("a", 43)), http.StatusBadRequest)

	// The fixture issuer would allow a second exchange, so exactly one
	// success demonstrates consumption in the shared store itself.
	state := location.Query().Get("state")
	type callbackResult struct {
		response *http.Response
		err      error
	}
	callbacks := make(chan callbackResult, 2)
	for _, server := range []string{two.URL, one.URL} {
		req := callback(server, state)
		go func() {
			response, err := browser.Do(req)
			callbacks <- callbackResult{response, err}
		}()
	}
	var cookie *http.Cookie
	var rejected int
	for range 2 {
		result := <-callbacks
		if result.err != nil {
			t.Fatal(result.err)
		}
		_ = result.response.Body.Close()
		switch result.response.StatusCode {
		case http.StatusSeeOther:
			if cookie != nil {
				t.Fatal("two replicas accepted the same OIDC transaction")
			}
			for _, value := range result.response.Cookies() {
				if value.Name == "cdn-manager-session" && value.HttpOnly && value.MaxAge > 0 {
					cookie = value
				}
			}
		case http.StatusBadRequest:
			rejected++
		default:
			t.Fatalf("unexpected callback status %d", result.response.StatusCode)
		}
	}
	if cookie == nil || rejected != 1 {
		t.Fatalf("callback did not issue one session and reject one replay, rejections=%d", rejected)
	}

	currentSession := func(server string) map[string]string {
		t.Helper()
		response := send(request(http.MethodGet, server+"/api/v1/session", cookie, ""), http.StatusOK)
		var value map[string]string
		if err := json.NewDecoder(response.Body).Decode(&value); err != nil || value["subject"] != "alice" || value["csrfToken"] == "" {
			t.Fatalf("invalid session data: %v", err)
		}
		return value
	}
	csrf := currentSession(one.URL)["csrfToken"]
	if other := currentSession(two.URL)["csrfToken"]; other != csrf {
		t.Fatal("replicas disagree on CSRF token")
	}
	send(request(http.MethodPost, two.URL+"/auth/logout", cookie, "wrong-token"), http.StatusForbidden)
	currentSession(one.URL)

	bearer := i.issue("at+jwt", map[string]any{"aud": "cdn-mcp", "scope": "read write"})
	authRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	authRequest.Header.Set("Authorization", "Bearer "+bearer)
	token, err := first.AuthorizeMCP(authRequest)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(500 * time.Millisecond)
	grant, deadline, err := first.IssueUploadGrant(t.Context(), token, "fixture-upload", 2, 123, expires)
	if err != nil || !deadline.Equal(expires) {
		t.Fatalf("grant issue or deadline failed: %v", err)
	}
	scopedRequest := request(http.MethodPut, two.URL+TransferPath, nil, "")
	scopedRequest.Header.Set("Authorization", "Bearer "+grant)
	for range 2 {
		value, err := second.UploadGrant(scopedRequest)
		if err != nil || value.UploadID != "fixture-upload" || value.Number != 2 || value.Size != 123 ||
			value.Actor.Subject != "alice" || !slices.Equal(value.Actor.Scopes, []string{service.Write}) {
			t.Fatalf("wrong transfer grant scope: %v", err)
		}
	}
	send(scopedRequest, http.StatusBadRequest) // endpoint accepted the grant and checked content length
	time.Sleep(time.Until(expires.Add(10 * time.Millisecond)))
	send(scopedRequest, http.StatusUnauthorized)

	one.Close()
	first, one = newReplica(clients[0])
	if currentSession(one.URL)["csrfToken"] != csrf {
		t.Fatal("session changed after an application restart")
	}
	send(request(http.MethodPost, two.URL+"/auth/logout", cookie, csrf), http.StatusNoContent)
	send(request(http.MethodGet, one.URL+"/api/v1/session", cookie, ""), http.StatusUnauthorized)
	send(request(http.MethodGet, two.URL+"/api/v1/session", cookie, ""), http.StatusUnauthorized)
	_ = clients[0].Close()
	send(request(http.MethodGet, one.URL+"/api/v1/session", cookie, ""), http.StatusServiceUnavailable)
	send(request(http.MethodGet, one.URL+"/auth/login", nil, ""), http.StatusServiceUnavailable)
	send(request(http.MethodPost, one.URL+"/auth/logout", cookie, csrf), http.StatusServiceUnavailable)
	if _, _, err := first.IssueUploadGrant(t.Context(), token, "fixture-upload", 2, 123, time.Now().Add(time.Minute)); !errors.Is(err, auth.ErrStoreUnavailable) {
		t.Fatalf("grant issuance failed open with disconnected state store: %v", err)
	}
	send(request(http.MethodGet, two.URL+"/api/v1/session", cookie, ""), http.StatusUnauthorized)
}
