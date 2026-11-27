package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisTimeout   = 3 * time.Second
	maxPayloadSize = 16 << 10
)

// The sorted set counts live credentials without scanning keys or relying on a
// notification for each expiry. Count and update it in the same operation.
var issueCredential = redis.NewScript(`
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local expires = tonumber(ARGV[1])
if expires <= now then return 0 end

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
local count = redis.call('ZCARD', KEYS[1])
local replace = KEYS[2] ~= KEYS[3] and redis.call('EXISTS', KEYS[3]) == 1
if replace then count = count - 1 end
if count >= tonumber(ARGV[2]) then return 0 end

if not redis.call('SET', KEYS[2], ARGV[3], 'PXAT', expires, 'NX') then return 0 end
local added = redis.pcall('ZADD', KEYS[1], expires, KEYS[2])
if type(added) == 'table' and added.err then
    redis.call('DEL', KEYS[2])
    return added
end
if replace then
    redis.call('DEL', KEYS[3])
    redis.call('ZREM', KEYS[1], KEYS[3])
end
redis.call('PEXPIREAT', KEYS[1], expires, 'GT')
redis.call('PEXPIREAT', KEYS[1], expires, 'NX')
return 1
`)

// A mismatched state must leave the transaction available for the legitimate
// callback. A matching state is consumed before making the OAuth exchange.
var consumeCredential = redis.NewScript(`
local data = redis.call('GET', KEYS[2])
if not data or string.sub(data, 1, 64) ~= ARGV[1] then return false end
redis.call('DEL', KEYS[2])
redis.call('ZREM', KEYS[1], KEYS[2])
return data
`)

var revokeCredential = redis.NewScript(`
redis.call('DEL', KEYS[2])
return redis.call('ZREM', KEYS[1], KEYS[2])
`)

func (s *expiringStore[T]) redisKey(hash [sha256.Size]byte) string {
	return s.prefix + "token:" + hex.EncodeToString(hash[:])
}

func (s *expiringStore[T]) issueRedis(ctx context.Context, hash, replaced [sha256.Size]byte, replace bool, item entry[T]) error {
	payload, err := json.Marshal(item)
	if err != nil || len(payload) > maxPayloadSize {
		return ErrStoreUnavailable
	}
	key := s.redisKey(hash)
	oldKey := key
	if replace {
		oldKey = s.redisKey(replaced)
	}
	// Match by hash so the OIDC state is not itself in the store. Sessions and
	// grants leave this field unused and use ordinary GET, never consume.
	data := hex.EncodeToString(item.match[:]) + string(payload)
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	added, err := issueCredential.Run(ctx, s.redis, []string{s.prefix + "active", key, oldKey}, item.Expires.UnixMilli(), s.limit, data).Int()
	if err != nil || added != 1 {
		return ErrStoreUnavailable
	}
	return nil
}

func (s *expiringStore[T]) getRedis(ctx context.Context, hash, match [sha256.Size]byte, consume bool) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	key := s.redisKey(hash)
	var data string
	var err error
	if consume {
		data, err = consumeCredential.Run(ctx, s.redis, []string{s.prefix + "active", key}, hex.EncodeToString(match[:])).Text()
	} else {
		data, err = s.redis.Get(ctx, key).Result()
	}
	var empty T
	if errors.Is(err, redis.Nil) {
		return empty, ErrUnauthorized
	}
	var item entry[T]
	if err != nil || len(data) <= sha256.Size*2 || json.Unmarshal([]byte(data[sha256.Size*2:]), &item) != nil {
		return empty, ErrStoreUnavailable
	}
	if !time.Now().Before(item.Expires) {
		return empty, ErrUnauthorized
	}
	return item.Value, nil
}

func (s *expiringStore[T]) removeRedis(ctx context.Context, hash [sha256.Size]byte) error {
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	if err := revokeCredential.Run(ctx, s.redis, []string{s.prefix + "active", s.redisKey(hash)}).Err(); err != nil {
		return ErrStoreUnavailable
	}
	return nil
}
