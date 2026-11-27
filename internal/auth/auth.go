package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
)

const (
	MCPPath      = "/mcp"
	MetadataPath = "/.well-known/oauth-protected-resource" + MCPPath
	loginTTL     = 5 * time.Minute
	defaultTTL   = 8 * time.Hour
)

var (
	ErrUnauthorized     = errors.New("authentication required")
	ErrStoreUnavailable = errors.New("authentication storage unavailable")
)

type Config struct {
	PublicURL              string
	OIDCIssuer             string
	OIDCClientID           string
	OIDCClientSecret       string
	MCPIssuer              string
	MCPAudience            string
	MCPScopes              []string
	MCPBrokerKey           []byte
	SessionTTL             time.Duration
	AllowInsecureLocalhost bool
	Redis                  redis.UniversalClient
	RedisPrefix            string
}

type Provider struct {
	publicURL, mcpIssuer, mcpAudience string
	mcpScopes                         []string
	secureCookies                     bool
	sessionTTL                        time.Duration
	httpClient                        *http.Client
	oauth                             oauth2.Config
	webVerifier, mcpVerifier          *oidc.IDTokenVerifier
	logins                            expiringStore[login]
	sessions                          expiringStore[session]
	uploadGrants                      expiringStore[UploadGrant]
	broker                            *mcpBroker
}

func New(ctx context.Context, cfg Config) (*Provider, error) {
	public, err := providerURL(cfg.PublicURL, cfg.AllowInsecureLocalhost)
	if err != nil || (public.Path != "" && public.Path != "/") {
		return nil, errors.New("public URL must be an HTTPS origin or an explicitly enabled HTTP loopback origin")
	}
	public.Path = ""
	public.RawPath = ""
	if cfg.OIDCClientID == "" || cfg.OIDCClientSecret == "" {
		return nil, errors.New("web OIDC client ID and secret are required")
	}
	if cfg.MCPIssuer == "" {
		cfg.MCPIssuer = cfg.OIDCIssuer
	}
	if len(cfg.MCPBrokerKey) != 0 && len(cfg.MCPBrokerKey) != 32 {
		return nil, errors.New("MCP OAuth broker key must contain 32 bytes")
	}
	if len(cfg.MCPBrokerKey) == 0 && (cfg.MCPAudience == "" || cfg.MCPAudience == cfg.OIDCClientID) {
		return nil, errors.New("MCP OAuth requires a named audience distinct from the web OIDC client ID")
	}
	if len(cfg.MCPScopes) == 0 {
		cfg.MCPScopes = []string{"read", "write"}
	}
	if !slices.Contains(cfg.MCPScopes, service.Read) {
		return nil, errors.New("MCP OAuth scopes must include read")
	}
	for _, scope := range cfg.MCPScopes {
		if !slices.Contains([]string{service.Read, service.Write, service.Delete}, scope) {
			return nil, fmt.Errorf("unknown MCP scope %q", scope)
		}
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = defaultTTL
	}
	if cfg.SessionTTL < time.Second || cfg.SessionTTL > 24*time.Hour {
		return nil, errors.New("web session lifetime must be between one second and 24 hours")
	}
	allowHTTP := cfg.AllowInsecureLocalhost && public.Scheme == "http"
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if first := via[0].URL; req.URL.Scheme != first.Scheme || req.URL.Host != first.Host || req.URL.User != nil || len(via) >= 10 {
			return errors.New("unsafe OIDC endpoint redirect")
		}
		return nil
	}}
	ctx = oidc.ClientContext(ctx, client)
	webProvider, err := discover(ctx, cfg.OIDCIssuer, allowHTTP)
	if err != nil {
		return nil, fmt.Errorf("discover web OIDC provider: %w", err)
	}
	p := &Provider{
		publicURL:     public.String(),
		mcpIssuer:     cfg.MCPIssuer,
		mcpAudience:   cfg.MCPAudience,
		mcpScopes:     slices.Clone(cfg.MCPScopes),
		secureCookies: public.Scheme == "https",
		sessionTTL:    cfg.SessionTTL,
		httpClient:    client,
		oauth: oauth2.Config{
			ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			RedirectURL: public.String() + "/auth/callback", Endpoint: webProvider.Endpoint(),
			Scopes: []string{oidc.ScopeOpenID, "profile"},
		},
		webVerifier:  webProvider.VerifierContext(ctx, &oidc.Config{ClientID: cfg.OIDCClientID}),
		logins:       newExpiringStore[login](cfg.Redis, cfg.RedisPrefix, "login", 4096),
		sessions:     newExpiringStore[session](cfg.Redis, cfg.RedisPrefix, "session", 8192),
		uploadGrants: newExpiringStore[UploadGrant](cfg.Redis, cfg.RedisPrefix, "grant", 8192),
	}
	if len(cfg.MCPBrokerKey) != 0 {
		p.mcpIssuer = p.publicURL + MCPBrokerPath
		p.mcpAudience = p.publicURL + MCPPath
		p.broker = newMCPBroker(p, cfg)
	} else {
		mcpProvider := webProvider
		if cfg.MCPIssuer != cfg.OIDCIssuer {
			mcpProvider, err = discover(ctx, cfg.MCPIssuer, allowHTTP)
			if err != nil {
				return nil, fmt.Errorf("discover MCP OAuth provider: %w", err)
			}
		}
		p.mcpVerifier = mcpProvider.VerifierContext(ctx, &oidc.Config{ClientID: cfg.MCPAudience})
	}
	return p, nil
}

func providerURL(raw string, allowLoopback bool) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("invalid URL")
	}
	if parsed.Scheme == "https" || (allowLoopback && parsed.Scheme == "http" &&
		(strings.EqualFold(parsed.Hostname(), "localhost") || net.ParseIP(parsed.Hostname()).IsLoopback())) {
		return parsed, nil
	}
	return nil, errors.New("HTTPS required")
}

func discover(ctx context.Context, issuer string, allowHTTP bool) (*oidc.Provider, error) {
	if _, err := providerURL(issuer, allowHTTP); err != nil {
		return nil, err
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, err
	}
	for _, endpoint := range []string{provider.Endpoint().AuthURL, provider.Endpoint().TokenURL, metadata.JWKS} {
		if _, err := providerURL(endpoint, allowHTTP); err != nil {
			return nil, fmt.Errorf("invalid OIDC endpoint: %w", err)
		}
	}
	return provider, nil
}

type identityClaims struct {
	Name            string   `json:"name"`
	Username        string   `json:"preferred_username"`
	Groups          []string `json:"groups"`
	AuthorizedParty string   `json:"azp"`
}

func (c identityClaims) actor(subject, client string) service.Actor {
	name := c.Name
	if name == "" {
		name = c.Username
	}
	return service.Actor{Subject: subject, Name: name, Client: client, Groups: c.Groups}
}

func (p *Provider) PublicURL() string { return p.publicURL }

// MCPConnection contains the non-secret values needed to configure an MCP client.
type MCPConnection struct {
	URL         string   `json:"url"`
	ClientID    string   `json:"clientId,omitempty"`
	Issuer      string   `json:"issuer"`
	MetadataURL string   `json:"metadataUrl"`
	Scopes      []string `json:"scopes"`
}

func (p *Provider) MCPConnection() MCPConnection {
	clientID := p.mcpAudience
	if p.broker != nil {
		clientID = ""
	}
	return MCPConnection{
		URL:         p.publicURL + MCPPath,
		ClientID:    clientID,
		Issuer:      p.mcpIssuer,
		MetadataURL: p.publicURL + MetadataPath,
		Scopes:      slices.Clone(p.mcpScopes),
	}
}

// AccessToken is a verified MCP access token reduced to application authorization data.
type AccessToken struct {
	Actor   service.Actor
	Expires time.Time
}

func (p *Provider) AuthorizeMCP(r *http.Request) (AccessToken, error) {
	raw := bearerToken(r)
	if raw == "" || len(raw) > 16<<10 {
		return AccessToken{}, ErrUnauthorized
	}
	if p.broker != nil {
		return p.broker.accessToken(r.Context(), raw)
	}
	ctx := oidc.ClientContext(r.Context(), p.httpClient)
	verified, err := p.mcpVerifier.Verify(ctx, raw)
	if err != nil || verified.Subject == "" || verified.Expiry.IsZero() ||
		verified.Issuer != p.mcpIssuer || len(verified.Audience) != 1 {
		return AccessToken{}, ErrUnauthorized
	}
	var claims struct {
		identityClaims
		Scope string `json:"scope"`
		UID   string `json:"uid"`
	}
	if err := verified.Claims(&claims); err != nil || claims.Scope == "" {
		return AccessToken{}, ErrUnauthorized
	}
	// Authentik's JWTs carry azp and uid specifically on access tokens. Also
	// accept explicitly typed OAuth JWT access tokens from compatible issuers.
	if !jwtAccessType(raw) && (claims.AuthorizedParty != p.mcpAudience || claims.UID == "") {
		return AccessToken{}, ErrUnauthorized
	}
	actor := claims.identityClaims.actor(verified.Subject, "mcp")
	for _, scope := range strings.Fields(claims.Scope) {
		if slices.Contains(p.mcpScopes, scope) && !slices.Contains(actor.Scopes, scope) {
			actor.Scopes = append(actor.Scopes, scope)
		}
	}
	return AccessToken{Actor: actor, Expires: verified.Expiry}, nil
}

func bearerToken(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) == 1 {
		parts := strings.Fields(values[0])
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return parts[1]
		}
	}
	return ""
}

func jwtAccessType(token string) bool {
	header, _, _ := strings.Cut(token, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return false
	}
	var fields struct {
		Type string `json:"typ"`
	}
	return json.Unmarshal(decoded, &fields) == nil && (strings.EqualFold(fields.Type, "at+jwt") || strings.EqualFold(fields.Type, "application/at+jwt"))
}

func (p *Provider) Bearer(r *http.Request) (service.Actor, error) {
	token, err := p.AuthorizeMCP(r)
	return token.Actor, err
}

func (p *Provider) Challenge(w http.ResponseWriter, r *http.Request, requiredScope string) {
	challenge := "Bearer resource_metadata=" + fmt.Sprintf("%q", p.publicURL+MetadataPath)
	status := http.StatusUnauthorized
	if requiredScope != "" {
		challenge += `, error="insufficient_scope", scope=` + fmt.Sprintf("%q", requiredScope)
		status = http.StatusForbidden
	} else {
		challenge += `, scope="read"`
		if len(r.Header.Values("Authorization")) > 0 {
			challenge += `, error="invalid_token"`
		}
	}
	w.Header().Set("WWW-Authenticate", challenge)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, http.StatusText(status), status)
}

func (p *Provider) Metadata(w http.ResponseWriter, _ *http.Request) {
	if p.broker != nil {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Resource               string   `json:"resource"`
		AuthorizationServers   []string `json:"authorization_servers"`
		BearerMethodsSupported []string `json:"bearer_methods_supported"`
		ScopesSupported        []string `json:"scopes_supported"`
	}{p.publicURL + MCPPath, []string{p.mcpIssuer}, []string{"header"}, p.mcpScopes})
}

func sameToken(left, right string) bool {
	return left != "" && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
