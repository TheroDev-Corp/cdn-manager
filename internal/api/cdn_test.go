package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
	"github.com/go-chi/chi/v5"
)

func TestCDNStatusUsesOnlyTheConfiguredGateway(t *testing.T) {
	var unexpected atomic.Int32
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unexpected.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer s3.Close()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("manager credentials were forwarded to the CDN")
		}
		switch r.URL.Path {
		case "/media/nested/猫 #%.txt":
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
			} else if r.Header.Get("Range") == "bytes=0-0" {
				w.Header().Set("Content-Range", "bytes 0-0/20")
				w.WriteHeader(http.StatusPartialContent)
				io.WriteString(w, "a")
			} else {
				t.Error("fallback request lacked its single-byte range")
				w.WriteHeader(http.StatusBadRequest)
			}
		case "/media/redirect":
			http.Redirect(w, r, s3.URL+"/private", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cdn.Close()
	source := storage.SourceConfig{
		ID: "cdn", Endpoint: s3.URL, Region: "example", AccessKey: "fixture", SecretKey: "fixture",
		Buckets: []string{"files"}, PathStyle: true, CDNURL: cdn.URL + "/media/{key}",
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
	for _, tc := range []struct {
		key    string
		result service.CDNAvailability
	}{
		{"nested/猫 #%.txt", service.CDNAvailability{Available: true, Status: http.StatusPartialContent}},
		{"missing", service.CDNAvailability{Status: http.StatusNotFound}},
		{"redirect", service.CDNAvailability{Status: http.StatusFound}},
	} {
		params := url.Values{"source": {"cdn"}, "bucket": {"files"}, "key": {tc.key}, "url": {s3.URL}}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cdn-status?"+params.Encode(), nil)
		req.Header.Set("Cookie", "private=credential")
		req.Header.Set("Authorization", "Bearer fake-token")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		var result service.CDNAvailability
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || result != tc.result {
			t.Errorf("key %q: HTTP %d result %+v (decode %v), want %+v", tc.key, response.Code, result, err, tc.result)
		}
	}
	if unexpected.Load() != 0 {
		t.Error("checking CDN availability contacted S3")
	}
}
