package storage

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetBucketPolicyAllowsAnUnconfiguredPolicy(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		code := "NoSuchBucket"
		if r.URL.Path == "/existing" {
			code = "NoSuchBucketPolicy"
		}
		io.WriteString(w, "<Error><Code>"+code+"</Code></Error>")
	}))
	defer s3.Close()
	config := SourceConfig{
		ID: "test", Endpoint: s3.URL, Region: "test", AccessKey: "fixture", SecretKey: "fixture",
		Buckets: []string{"existing", "missing"}, PathStyle: true, Capabilities: Capabilities{BucketPolicies: true},
	}
	store, err := New([]SourceConfig{config}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, _ := store.Source(config.ID)
	if result, err := source.GetBucketPolicy(t.Context(), "existing"); err != nil || result != "" {
		t.Errorf("policy before creation = %q, %v", result, err)
	}
	if _, err := source.GetBucketPolicy(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing bucket error = %v, want ErrNotFound", err)
	}
}
