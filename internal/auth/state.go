package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const tokenLength = 43 // The unpadded base64url representation of 32 random bytes.

type entry[T any] struct {
	Value   T
	Expires time.Time
	match   [sha256.Size]byte
}

type expiringStore[T any] struct {
	redis     redis.UniversalClient
	prefix    string
	limit     int
	mu        sync.Mutex
	entries   map[[sha256.Size]byte]entry[T]
	nextPrune time.Time
}

func newExpiringStore[T any](client redis.UniversalClient, prefix, kind string, limit int) expiringStore[T] {
	if prefix == "" {
		prefix = "cdn-manager"
	}
	return expiringStore[T]{
		redis: client, prefix: prefix + ":auth:{" + kind + "}:", limit: limit,
		entries: make(map[[sha256.Size]byte]entry[T]),
	}
}

func newToken() string {
	var entropy [32]byte
	rand.Read(entropy[:])
	return base64.RawURLEncoding.EncodeToString(entropy[:])
}

func (s *expiringStore[T]) issue(ctx context.Context, value T, until time.Time, replaces, match string) (string, error) {
	id := newToken()
	if err := s.put(ctx, id, value, until, replaces, match); err != nil {
		return "", err
	}
	return id, nil
}

func (s *expiringStore[T]) put(ctx context.Context, id string, value T, until time.Time, replaces, match string) error {
	if !time.Now().Before(until) {
		return ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(id))
	replaced := sha256.Sum256([]byte(replaces))
	item := entry[T]{Value: value, Expires: until, match: sha256.Sum256([]byte(match))}
	replace := len(replaces) == tokenLength
	if s.redis != nil {
		return s.issueRedis(ctx, hash, replaced, replace, item)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !now.Before(s.nextPrune) || len(s.entries) >= s.limit {
		for key, stored := range s.entries {
			if !now.Before(stored.Expires) {
				delete(s.entries, key)
			}
		}
		s.nextPrune = now.Add(time.Minute)
	}
	count := len(s.entries)
	if _, exists := s.entries[replaced]; replace && exists {
		count--
	}
	if _, exists := s.entries[hash]; exists || count >= s.limit || !now.Before(until) {
		return ErrStoreUnavailable
	}
	if replace {
		delete(s.entries, replaced)
	}
	s.entries[hash] = item
	return nil
}

func (s *expiringStore[T]) get(ctx context.Context, id, match string, consume bool) (T, error) {
	var empty T
	if len(id) != tokenLength || (consume && len(match) != tokenLength) {
		return empty, ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(id))
	return s.getHashed(ctx, hash, match, consume)
}

func (s *expiringStore[T]) getHashed(ctx context.Context, hash [sha256.Size]byte, match string, consume bool) (T, error) {
	var empty T
	expected := sha256.Sum256([]byte(match))
	if s.redis != nil {
		return s.getRedis(ctx, hash, expected, consume)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	item, exists := s.entries[hash]
	if !exists {
		return empty, ErrUnauthorized
	}
	if !time.Now().Before(item.Expires) {
		delete(s.entries, hash)
		return empty, ErrUnauthorized
	}
	if consume {
		if subtle.ConstantTimeCompare(item.match[:], expected[:]) != 1 {
			return empty, ErrUnauthorized
		}
		delete(s.entries, hash)
	}
	return item.Value, nil
}

func (s *expiringStore[T]) remove(ctx context.Context, id string) error {
	if len(id) != tokenLength {
		return nil
	}
	hash := sha256.Sum256([]byte(id))
	if s.redis != nil {
		return s.removeRedis(ctx, hash)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, hash)
	return nil
}
