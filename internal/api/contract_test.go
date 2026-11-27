package api

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v2"
)

type missingSession struct{}

func (missingSession) Web(*http.Request) (service.Actor, string, error) {
	return service.Actor{}, "", errors.New("session required")
}

func (missingSession) ValidCSRF(*http.Request, string, string) bool {
	return false
}

func (missingSession) MCPConnection() auth.MCPConnection { return auth.MCPConnection{} }

type operationContract struct {
	ID          string `yaml:"operationId"`
	Responses   map[string]any
	Security    []map[string][]string
	RequestBody struct {
		Content map[string]any
	} `yaml:"requestBody"`
}

func TestRESTContract(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		OpenAPI string `yaml:"openapi"`
		Paths   map[string]map[string]any
		Extra   map[string]any `yaml:",inline"`
	}
	if err := yaml.UnmarshalStrict(body, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.OpenAPI != "3.1.0" {
		t.Fatalf("unexpected OpenAPI version %q", spec.OpenAPI)
	}
	operations := make(map[string]operationContract)
	ids := make(map[string]bool)
	for path, methods := range spec.Paths {
		for method, operation := range methods {
			if method == "parameters" {
				continue
			}
			body, err := yaml.Marshal(operation)
			if err != nil {
				t.Fatal(err)
			}
			var contract operationContract
			if err := yaml.Unmarshal(body, &contract); err != nil {
				t.Fatal(err)
			}
			if contract.ID == "" || ids[contract.ID] {
				t.Fatalf("duplicate or missing operation ID for %s %s", method, path)
			}
			ids[contract.ID] = true
			operations[strings.ToUpper(method)+" "+path] = contract
		}
	}

	unauthenticated, authenticated := chi.NewRouter(), chi.NewRouter()
	Mount(unauthenticated, nil, missingSession{})
	Mount(authenticated, nil, fixtureAuth{})
	if err := chi.Walk(unauthenticated, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + pattern
		op, ok := operations[key]
		if !ok {
			t.Errorf("route missing from specification: %s", key)
			return nil
		}
		delete(operations, key)
		target := strings.NewReplacer("{id}", "AAAAAAAAAAAAAAAAAAAAAAAAAA", "{number}", "1").Replace(pattern)
		t.Run(op.ID, func(t *testing.T) {
			assertErrorResponse(t, unauthenticated, method, target, http.StatusUnauthorized)
			if _, ok := op.Responses["401"]; !ok {
				t.Fatal("missing 401 response in specification")
			}
			if method == http.MethodHead || method == http.MethodGet {
				return
			}
			assertErrorResponse(t, authenticated, method, target, http.StatusForbidden)
			if _, ok := op.Responses["403"]; !ok {
				t.Error("missing 403 response in specification")
			}
			if len(op.Security) == 0 || !requiresBoth(op.Security[0], "webSession", "csrfHeader") {
				t.Error("missing browser-session and CSRF security requirements")
			}
			if pattern == "/api/v1/zip" && op.RequestBody.Content["application/x-www-form-urlencoded"] == nil {
				t.Error("missing native form ZIP body")
			}
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for key := range operations {
		t.Errorf("specification mentions an unregistered route: %s", key)
	}
}

func requiresBoth(requirements map[string][]string, first, second string) bool {
	_, hasFirst := requirements[first]
	_, hasSecond := requirements[second]
	return hasFirst && hasSecond
}

func assertErrorResponse(t *testing.T, router http.Handler, method, target string, status int) {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != status {
		t.Fatalf("status = %d, want %d", w.Code, status)
	}
	contentType, _, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		t.Fatalf("content type = %q, %v", contentType, err)
	}
	var response struct{ Message string }
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Message == "" {
		t.Fatalf("invalid REST error envelope: %s", w.Body.String())
	}
}
