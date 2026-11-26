package storage

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHealthSupportsReservedBucketNames(t *testing.T) {
	var unavailable atomic.Bool
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListAllMyBucketsResult><Buckets><Bucket><Name>existing</Name></Bucket></Buckets></ListAllMyBucketsResult>")
		case "/existing":
			w.WriteHeader(http.StatusOK)
		case "/reserved":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected S3 request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer s3.Close()

	fixed := SourceConfig{
		ID: "fixed", Endpoint: s3.URL, Region: "test", AccessKey: "fixture", SecretKey: "fixture",
		Buckets: []string{"existing", "reserved"}, PathStyle: true,
	}
	managed := fixed
	managed.ID = "managed"
	managed.Capabilities.BucketManagement = true
	store, err := New([]SourceConfig{fixed, managed}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	managedSource, _ := store.Source(managed.ID)
	if err := managedSource.Health(t.Context()); err != nil {
		t.Fatalf("managed source with a reserved name is available: %v", err)
	}
	fixedSource, _ := store.Source(fixed.ID)
	if err := fixedSource.Health(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fixed source with a missing bucket should be unavailable: %v", err)
	}
	unavailable.Store(true)
	if err := managedSource.Health(t.Context()); err == nil {
		t.Error("managed source with denied bucket listing was marked available")
	}
	if err := fixedSource.Health(t.Context()); err == nil {
		t.Error("fixed source with denied bucket access was marked available")
	}
}
