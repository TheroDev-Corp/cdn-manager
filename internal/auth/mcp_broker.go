package auth

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/go-chi/chi/v5"
)

const (
	MCPBrokerPath    = MCPPath + "/auth"
	brokerRequestTTL = 10 * time.Minute
	brokerCodeTTL    = time.Minute
	brokerAccessTTL  = 15 * time.Minute
	brokerBodyLimit  = 16 << 10
)

type mcpBroker struct {
	provider       *Provider
	key            []byte
	metadataClient *http.Client
	requests       expiringStore[brokerRequest]
	codes          expiringStore[brokerCode]
	grants         expiringStore[brokerGrant]
	access         expiringStore[brokerToken]
	refresh        expiringStore[struct{}]
	refreshHistory expiringStore[brokerRefresh]
}

type brokerRequest struct {
	ClientID, Name, Redirect, State, Challenge, Resource string
	Scopes                                               []string
	Refresh                                              bool
}

type brokerCode struct {
	ClientID, Redirect, Challenge, Resource string
	Session                                 [sha256.Size]byte
	Scopes                                  []string
	Refresh                                 bool
}

type brokerGrant struct {
	ClientID, Resource string
	Session            [sha256.Size]byte
	Expires            time.Time
}

type brokerToken struct {
	Grant   string
	Expires time.Time
	Scopes  []string
}

func newMCPBroker(p *Provider, cfg Config) *mcpBroker {
	return &mcpBroker{
		provider: p, key: slices.Clone(cfg.MCPBrokerKey), metadataClient: publicMetadataClient(),
		requests:       newExpiringStore[brokerRequest](cfg.Redis, cfg.RedisPrefix, "mcp-request", 4096),
		codes:          newExpiringStore[brokerCode](cfg.Redis, cfg.RedisPrefix, "mcp-code", 4096),
		grants:         newExpiringStore[brokerGrant](cfg.Redis, cfg.RedisPrefix, "mcp-grant", 8192),
		access:         newExpiringStore[brokerToken](cfg.Redis, cfg.RedisPrefix, "mcp-access", 32768),
		refresh:        newExpiringStore[struct{}](cfg.Redis, cfg.RedisPrefix, "mcp-refresh", 8192),
		refreshHistory: newExpiringStore[brokerRefresh](cfg.Redis, cfg.RedisPrefix, "mcp-refresh-history", 65536),
	}
}

func (p *Provider) MountMCPBroker(router chi.Router) {
	b := p.broker
	if b == nil {
		return
	}
	metadata := "/.well-known/oauth-authorization-server" + MCPBrokerPath
	router.Get(metadata, b.metadata)
	router.Options(metadata, brokerPreflight)
	router.Options(MetadataPath, brokerPreflight)
	router.Get(MCPBrokerPath+"/authorize", b.authorize)
	router.Get(MCPBrokerPath+"/consent", b.consent)
	router.Post(MCPBrokerPath+"/consent", b.consent)
	for path, handler := range map[string]http.HandlerFunc{"/register": b.register, "/token": b.token, "/revoke": b.revoke} {
		router.Post(MCPBrokerPath+path, handler)
		router.Options(MCPBrokerPath+path, brokerPreflight)
	}
}

func brokerPreflight(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

func brokerJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func brokerError(w http.ResponseWriter, status int, code string) {
	brokerJSON(w, status, map[string]string{"error": code})
}

func (b *mcpBroker) metadata(w http.ResponseWriter, _ *http.Request) {
	issuer := b.provider.mcpIssuer
	brokerJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         issuer,
		"authorization_endpoint":                         issuer + "/authorize",
		"token_endpoint":                                 issuer + "/token",
		"registration_endpoint":                          issuer + "/register",
		"revocation_endpoint":                            issuer + "/revoke",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"authorization_response_iss_parameter_supported": true,
		"client_id_metadata_document_supported":          true,
		"scopes_supported":                               b.provider.mcpScopes,
	})
}

func setBrokerPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'; style-src 'unsafe-inline'")
}

func (b *mcpBroker) authorize(w http.ResponseWriter, r *http.Request) {
	setBrokerPageHeaders(w)
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > brokerBodyLimit || !singleBrokerValues(q) || q.Get("response_type") != "code" ||
		(q.Get("response_mode") != "" && q.Get("response_mode") != "query") || len(q.Get("state")) > 1024 ||
		q.Get("code_challenge_method") != "S256" || !validChallenge(q.Get("code_challenge")) {
		brokerError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !b.validResource(q.Get("resource")) {
		brokerError(w, http.StatusBadRequest, "invalid_target")
		return
	}
	client, err := b.client(r.Context(), q.Get("client_id"))
	if err != nil || !client.acceptsRedirect(q.Get("redirect_uri")) {
		brokerError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	scopes := strings.Fields(q.Get("scope"))
	if len(scopes) == 0 {
		scopes = []string{service.Read}
	}
	for _, scope := range scopes {
		if !slices.Contains(b.provider.mcpScopes, scope) {
			brokerError(w, http.StatusBadRequest, "invalid_scope")
			return
		}
	}
	// Canonicalize scopes in server order and discard duplicates.
	requested := make([]string, 0, len(b.provider.mcpScopes))
	for _, scope := range b.provider.mcpScopes {
		if slices.Contains(scopes, scope) && !slices.Contains(requested, scope) {
			requested = append(requested, scope)
		}
	}
	request := brokerRequest{ClientID: q.Get("client_id"), Name: client.Name, Redirect: q.Get("redirect_uri"),
		State: q.Get("state"), Challenge: q.Get("code_challenge"), Resource: b.provider.mcpAudience, Scopes: requested, Refresh: client.canRefresh()}
	id := newToken()
	if err := b.requests.put(r.Context(), id, request, time.Now().Add(brokerRequestTTL), "", id); err != nil {
		brokerError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	http.Redirect(w, r, MCPBrokerPath+"/consent?request="+id, http.StatusSeeOther)
}

func singleBrokerValues(values url.Values) bool {
	for _, items := range values {
		if len(items) != 1 {
			return false
		}
	}
	return true
}

func (b *mcpBroker) validResource(resource string) bool {
	// Older MCP clients omit the indicator. This issuer serves exactly one resource.
	return resource == "" || resource == b.provider.mcpAudience
}

func (b *mcpBroker) response(w http.ResponseWriter, r *http.Request, request brokerRequest, key, value string) {
	target, _ := url.Parse(request.Redirect)
	query := target.Query()
	query.Set(key, value)
	query.Set("iss", b.provider.mcpIssuer)
	if request.State != "" {
		query.Set("state", request.State)
	}
	target.RawQuery = query.Encode()
	// Issuer and response parameters go only to a validated callback after an
	// explicit browser action on the consent page.
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

var mcpConsentPage = template.Must(template.New("mcp-consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connect to CDN Manager</title>
<style>body{font:16px system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;color:#191713}header{border-bottom:3px solid #f40}form{display:grid;gap:1rem}fieldset{display:grid;gap:.65rem;min-width:0}code{overflow-wrap:anywhere}button{font:inherit;padding:.55rem .8rem}footer{display:flex;gap:.75rem}</style></head>
<body><header><strong>CDN Manager</strong><h1>Allow {{.Name}} to connect?</h1></header>
<p>Signed in as {{.Account}}. The browser will return to <code>{{.Host}}</code>. Continue only if you started this connection.</p>
<form method="post"><input type="hidden" name="request" value="{{.ID}}"><input type="hidden" name="csrf" value="{{.CSRF}}">
<fieldset><legend>Access</legend>{{range .Scopes}}<label><input type="checkbox" name="scope" value="{{.Value}}"{{if .Default}} checked{{end}}> {{.Label}}</label>{{end}}</fieldset>
<footer><button name="decision" value="approve">Allow access</button><button name="decision" value="deny">Cancel</button></footer></form></body></html>`))

func (b *mcpBroker) consent(w http.ResponseWriter, r *http.Request) {
	setBrokerPageHeaders(w)
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, brokerBodyLimit)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid consent form", http.StatusBadRequest)
			return
		}
	}
	id := r.URL.Query().Get("request")
	if r.Method == http.MethodPost {
		id = r.PostForm.Get("request")
	}
	request, err := b.requests.get(r.Context(), id, "", false)
	if err != nil || request.Resource != b.provider.mcpAudience {
		http.Error(w, "MCP authorization request is unavailable", http.StatusBadRequest)
		return
	}
	actor, csrf, err := b.provider.Web(r)
	if errors.Is(err, ErrUnauthorized) && r.Method == http.MethodGet {
		http.Redirect(w, r, "/auth/login?mcp_consent="+id, http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Error(w, "Sign-in required", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		b.renderConsent(w, request, id, csrf, actor)
		return
	}
	if !b.provider.ValidCSRF(r, csrf, r.PostForm.Get("csrf")) {
		http.Error(w, "Invalid consent form", http.StatusForbidden)
		return
	}
	decision := r.PostForm.Get("decision")
	approved := r.PostForm["scope"]
	if decision != "approve" && decision != "deny" {
		http.Error(w, "Invalid consent decision", http.StatusBadRequest)
		return
	}
	granted := make([]string, 0, len(request.Scopes))
	for _, scope := range request.Scopes {
		if slices.Contains(approved, scope) {
			granted = append(granted, scope)
		}
	}
	if _, err := b.requests.get(r.Context(), id, id, true); err != nil {
		http.Error(w, "MCP authorization request is unavailable", http.StatusBadRequest)
		return
	}
	if decision == "deny" || len(granted) == 0 {
		b.response(w, r, request, "error", "access_denied")
		return
	}
	code := brokerCode{ClientID: request.ClientID, Redirect: request.Redirect, Challenge: request.Challenge, Resource: request.Resource,
		Session: sha256.Sum256([]byte(b.provider.cookie(r, "session"))), Scopes: granted, Refresh: request.Refresh}
	raw, err := b.codes.issue(r.Context(), code, time.Now().Add(brokerCodeTTL), "", request.Challenge)
	if err != nil {
		http.Error(w, "MCP authorization is temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	b.response(w, r, request, "code", raw)
}

func (b *mcpBroker) renderConsent(w http.ResponseWriter, request brokerRequest, id, csrf string, actor service.Actor) {
	target, _ := url.Parse(request.Redirect)
	// Keep the Origin on the same-origin form submission without forwarding
	// the consent URL to an external callback.
	w.Header().Set("Referrer-Policy", "same-origin")
	// Some browsers apply form-action to the subsequent redirect as well.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self' "+target.Scheme+"://"+target.Host+"; base-uri 'none'; frame-ancestors 'none'; style-src 'unsafe-inline'")
	account := actor.Name
	if account == "" {
		account = actor.Subject
	}
	type choice struct {
		Value, Label string
		Default      bool
	}
	labels := map[string]string{service.Read: "Read storage and objects", service.Write: "Write and upload objects", service.Delete: "Delete objects"}
	choices := make([]choice, 0, len(request.Scopes))
	for _, scope := range request.Scopes {
		choices = append(choices, choice{scope, labels[scope], scope == service.Read})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = mcpConsentPage.Execute(w, struct {
		Name, Account, Host, ID, CSRF string
		Scopes                        []choice
	}{request.Name, account, target.Host, id, csrf, choices})
}
