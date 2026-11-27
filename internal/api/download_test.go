package api

import (
	"archive/zip"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
	"github.com/go-chi/chi/v5"
)

type fixtureAuth struct{}

func (fixtureAuth) Web(*http.Request) (service.Actor, string, error) {
	return service.Actor{Subject: "fixture", Client: "web", Groups: []string{"readers"}}, "fixture-csrf", nil
}

func (fixtureAuth) ValidCSRF(_ *http.Request, expected, submitted string) bool {
	return expected == submitted
}

func (fixtureAuth) MCPConnection() auth.MCPConnection { return auth.MCPConnection{} }

func TestZIPRelativePathsAndCSRF(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, strings.TrimPrefix(r.URL.Path, "/files/"))
	}))
	defer s3.Close()
	source := storage.SourceConfig{
		ID: "test", Endpoint: s3.URL, Region: "test", AccessKey: "fixture", SecretKey: "fixture",
		Buckets: []string{"files"}, PathStyle: true,
	}
	store, err := storage.New([]storage.SourceConfig{source}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := service.New(store, []service.SourceConfig{{
		SourceConfig: source, Roles: service.Roles{Viewer: []string{"readers"}},
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := chi.NewRouter()
	Mount(router, backend, fixtureAuth{})

	keys := []string{"news/index.txt", "news/тест #%.txt"}
	for _, tc := range []struct {
		name, csrf string
		status     int
	}{{"forbidden", "invalid", http.StatusForbidden}, {"download", "fixture-csrf", http.StatusOK}} {
		t.Run(tc.name, func(t *testing.T) {
			body := url.Values{"csrfToken": []string{tc.csrf}, "keys": keys}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/zip?source=test&bucket=files", strings.NewReader(body.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d", response.Code, tc.status)
			}
			if tc.status != http.StatusOK {
				return
			}
			archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
			if err != nil {
				t.Fatal(err)
			}
			var actual []string
			for _, entry := range archive.File {
				actual = append(actual, entry.Name)
			}
			if !reflect.DeepEqual(actual, keys) {
				t.Fatalf("archive keys = %v, want %v", actual, keys)
			}
		})
	}
}

func TestArchivePathsDisambiguatesUnsafeAndCollidingNames(t *testing.T) {
	keys := []string{"one/a.txt", "one/b.txt", "ONE/A.TXT", "../a.txt", "one", "aux.txt", "D:/unsafe.txt"}
	paths := archivePaths(keys)
	if !reflect.DeepEqual(paths[:2], keys[:2]) {
		t.Fatalf("safe sibling paths changed: %v", paths[:2])
	}
	seen := make(map[string]bool)
	for _, name := range paths {
		folded := strings.ToLower(name)
		if !safeZIPPath(name) || seen[folded] {
			t.Errorf("unsafe or duplicate archive path %q", name)
		}
		seen[folded] = true
	}
}
