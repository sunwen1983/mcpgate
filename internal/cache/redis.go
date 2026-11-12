package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisOpTimeout bounds a single cache operation. A slow Redis must not
// stall the request path — a failed check fails open (see RedisCache).
const redisOpTimeout = 500 * time.Millisecond

// RedisCache is a TTL response cache whose entries live in Redis, so
// every gateway replica sharing that Redis sees the same cached
// responses instead of one copy per process. It has the same semantics
// as Cache: entries expire after ttl, Set with ttl <= 0 stores nothing,
// and the gateway's cache keys are already replica-independent
// (backend + method + canonical params), so no key translation is
// needed. Keys are namespaced as prefix + "cache:" + key, keeping them
// apart from the rate limiter's "ratelimit:" keys.
//
// A Redis error fails open: Get reports a miss and Set/ Delete drop
// the write, and the error goes to onError (when set) so the caller
// can count it. Caching is a performance optimization, not a
// correctness gate — a Redis outage must degrade to uncached backend
// calls, not take the gateway down with it.
type RedisCache struct {
	client  redis.Cmdable
	prefix  string // key prefix, e.g. "mcpgate:"
	onError func(error)
}

// NewRedis builds a RedisCache. Errors are ignored when no error
// handler is given; use NewRedisWithOnError to observe them.
func NewRedis(client redis.Cmdable, keyPrefix string) *RedisCache {
	return NewRedisWithOnError(client, keyPrefix, nil)
}

// NewRedisWithOnError builds a RedisCache reporting Redis errors to
// onError (nil means errors are ignored).
func NewRedisWithOnError(client redis.Cmdable, keyPrefix string, onError func(error)) *RedisCache {
	return &RedisCache{client: client, prefix: keyPrefix, onError: onError}
}

// key returns the Redis key for a cache key.
func (c *RedisCache) key(key string) string {
	return c.prefix + "cache:" + key
}

// Get returns the cached value for key, or nil, false on a miss, an
// expired entry (Redis evicts it), or a Redis error.
func (c *RedisCache) Get(key string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	val, err := c.client.Get(ctx, c.key(key)).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) && c.onError != nil {
			c.onError(err)
		}
		return nil, false
	}
	return val, true
}

// Set stores value under key for ttl. A ttl <= 0 disables caching for
// the entry (nothing is stored). A Redis error drops the write.
func (c *RedisCache) Set(key string, value []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	if err := c.client.Set(ctx, c.key(key), value, ttl).Err(); err != nil {
		if c.onError != nil {
			c.onError(err)
		}
	}
}

// Delete removes key from the cache. A Redis error is reported and the
// entry may survive; invalidation is best-effort.
func (c *RedisCache) Delete(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	if err := c.client.Del(ctx, c.key(key)).Err(); err != nil {
		if c.onError != nil {
			c.onError(err)
		}
	}
}

// Clear removes every cache entry under this prefix and returns how
// many were removed. It scans instead of flushing: the Redis may be
// shared with other applications (and with the rate limiter), so only
// keys under prefix + "cache:" are touched.
func (c *RedisCache) Clear() int {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	pattern := c.prefix + "cache:*"
	var cursor uint64
	cleared := 0
	for {
		keys, next, err := c.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			if c.onError != nil {
				c.onError(err)
			}
			return cleared
		}
		if len(keys) > 0 {
			n, err := c.client.Del(ctx, keys...).Result()
			if err != nil {
				if c.onError != nil {
					c.onError(err)
				}
				return cleared
			}
			cleared += int(n)
		}
		cursor = next
		if cursor == 0 {
			return cleared
		}
	}
}
