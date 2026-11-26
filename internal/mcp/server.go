package mcp

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	TransferPath = "/mcp/upload-part"
	maxJSONBody  = 8 << 20
)

type Server struct {
	backend   *service.Backend
	provider  *auth.Provider
	transport http.Handler
	scopes    map[string]string
}

type tokenKey struct{}

func New(backend *service.Backend, provider *auth.Provider) *Server {
	s := &Server{backend: backend, provider: provider, scopes: make(map[string]string)}
	server := sdk.NewServer(&sdk.Implementation{Name: "cdn-manager", Version: "1"}, nil)
	registerTools(s, server)
	s.transport = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxJSONBody, PropagateRequestCancellation: true,
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.allowOrigin(r) {
		http.Error(w, "Forbidden origin", http.StatusForbidden)
		return
	}
	token, err := s.provider.AuthorizeMCP(r)
	if err != nil {
		s.provider.Challenge(w, r, "")
		return
	}
	if len(token.Actor.Scopes) == 0 {
		s.provider.Challenge(w, r, service.Read)
		return
	}
	// Clients using the 2026-07-28 protocol send the canonical tool name in
	// the header. The SDK verifies that this matches the body. Older clients
	// also get the per-tool check in addTool, independently of their headers.
	if r.Header.Get("Mcp-Method") == "tools/call" {
		if scope := s.scopes[r.Header.Get("Mcp-Name")]; scope != "" && !slices.Contains(token.Actor.Scopes, scope) {
			s.provider.Challenge(w, r, scope)
			return
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), tokenKey{}, token))
	s.transport.ServeHTTP(w, r)
}

func (s *Server) allowOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	return len(origins) == 0 || (len(origins) == 1 && origins[0] == s.provider.PublicURL())
}

func addTool[Input, Output any](s *Server, server *sdk.Server, name, description, scope string, invoke func(context.Context, auth.AccessToken, Input) (Output, error)) {
	s.scopes[name] = scope
	tool := &sdk.Tool{Name: name, Description: description}
	if scope == service.Read {
		tool.Annotations = &sdk.ToolAnnotations{ReadOnlyHint: true}
	}
	sdk.AddTool(server, tool, func(ctx context.Context, _ *sdk.CallToolRequest, input Input) (*sdk.CallToolResult, Output, error) {
		token, ok := ctx.Value(tokenKey{}).(auth.AccessToken)
		if !ok || !slices.Contains(token.Actor.Scopes, scope) {
			var empty Output
			return nil, empty, errors.New("OAuth scope required: " + scope)
		}
		output, err := invoke(ctx, token, input)
		if err != nil {
			var empty Output
			return nil, empty, clientError(err)
		}
		return nil, output, nil
	})
}

func clientError(err error) error {
	for _, known := range []error{service.ErrForbidden, storage.ErrInvalid, storage.ErrNotFound, storage.ErrConflict,
		storage.ErrUnsupported, storage.ErrTooLarge, storage.ErrBusy} {
		if errors.Is(err, known) {
			return known
		}
	}
	return errors.New("storage operation failed")
}
