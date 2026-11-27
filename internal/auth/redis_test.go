package auth

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisCredentialStoreCapacity(t *testing.T) {
	addr := os.Getenv("CDN_MANAGER_TEST_REDIS_URL")
	if addr == "" {
		t.Skip("set CDN_MANAGER_TEST_REDIS_URL to a disposable Redis or Valkey")
	}
	options, err := redis.ParseURL(addr)
	if err != nil {
		t.Fatal("invalid fixture Redis URL")
	}
	options.MaxRetries = -1
	prefix := "cdn-manager-test-capacity:" + time.Now().Format("20060102150405.000000000")
	clientOne, clientTwo := redis.NewClient(options), redis.NewClient(options)
	defer clientOne.Close()
	defer clientTwo.Close()
	first := newExpiringStore[string](clientOne, prefix, "capacity", 2)
	second := newExpiringStore[string](clientTwo, prefix, "capacity", 2)
	type issuance struct {
		token string
		err   error
	}
	results := make(chan issuance, 8)
	var workers sync.WaitGroup
	deadline := time.Now().Add(time.Second)
	for n := range cap(results) {
		store := &first
		if n%2 == 0 {
			store = &second
		}
		workers.Go(func() {
			token, err := store.issue(t.Context(), "value", deadline, "", "")
			results <- issuance{token, err}
		})
	}
	workers.Wait()
	close(results)
	var issued []string
	for result := range results {
		switch {
		case result.err == nil:
			issued = append(issued, result.token)
		case !errors.Is(result.err, ErrStoreUnavailable):
			t.Fatalf("unexpected state issuance error: %v", result.err)
		}
	}
	if len(issued) != 2 {
		t.Fatalf("expected exactly two shared credentials, got %d", len(issued))
	}
	replaced := issued[0]
	current, err := second.issue(t.Context(), "replacement", deadline, replaced, "")
	if err != nil {
		t.Fatalf("cannot rotate credential at capacity: %v", err)
	}
	if _, err := first.get(t.Context(), replaced, "", false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replaced credential is still available: %v", err)
	}
	if value, err := first.get(t.Context(), current, "", false); err != nil || value != "replacement" {
		t.Fatalf("replica cannot read replacement: %v", err)
	}
	time.Sleep(time.Until(deadline.Add(10 * time.Millisecond)))
	if _, err := first.get(t.Context(), current, "", false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired credential is still available: %v", err)
	}
	if count, err := clientTwo.Exists(t.Context(), second.prefix+"active").Result(); err != nil || count != 0 {
		t.Fatalf("expired capacity index was not reclaimed: count=%d, err=%v", count, err)
	}
}
