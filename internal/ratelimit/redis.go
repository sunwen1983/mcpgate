package ratelimit

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketScript atomically refills and consumes one token from the
// bucket stored at KEYS[1] (a hash with "tokens" and "updated_ms").
//
// ARGV: rate (tokens per second), burst, now (unix milliseconds),
// ttl (milliseconds the bucket key lives after this call).
//
// The refill is computed from the stored timestamp, so the bucket
// behaves exactly like the in-process Limiter; the whole
// read-modify-write runs inside one script, so concurrent replicas
// cannot overspend the budget. The key expires after (burst/rate + 1)
// seconds of idleness — long enough that a full refill always happens
// before expiry — so idle keys do not accumulate in Redis forever.
//
// Returns 1 when a token was consumed, 0 when the bucket is empty.
const tokenBucketScript = `
local tokens = tonumber(redis.call('HGET', KEYS[1], 'tokens'))
local updated = tonumber(redis.call('HGET', KEYS[1], 'updated_ms'))
local now = tonumber(ARGV[3])
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
if tokens == nil then
  tokens = burst
  updated = now
end
local elapsed = math.max(0, now - updated) / 1000
tokens = math.min(burst, tokens + elapsed * rate)
local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'updated_ms', now)
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[4]))
return allowed
`

// Dial connects to Redis and verifies it answers PING. The returned
// client is shared by every RedisLimiter the gateway creates. Retries
// are disabled and timeouts are short: a slow Redis must not stall the
// request path — a failed check fails open (see RedisLimiter.Allow).
//
// The TCP probe runs first because go-redis retries dials internally
// even with MaxRetries 0: probing the port directly makes an
// unreachable Redis fail in milliseconds instead of after seconds of
// retried dials.
func Dial(addr, password string, db int) (*redis.Client, error) {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("redis dial %s: %w", addr, err)
	}
	_ = conn.Close()
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		MaxRetries:   0,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	return client, nil
}

// RedisLimiter is a token-bucket limiter whose buckets live in Redis,
// so every gateway replica sharing that Redis enforces one budget per
// key instead of one budget per process. It has the same semantics as
// Limiter: up to Burst tokens, refilling at Rate tokens per second.
//
// A Redis error fails open: Allow returns true and reports the error
// to onError (when set) so the caller can count it. Rate limiting is
// a protective control, not a correctness gate — a Redis outage must
// degrade limiting, not take the gateway down with it.
type RedisLimiter struct {
	client  redis.Cmdable
	rate    float64 // tokens refilled per second
	burst   int     // bucket capacity
	prefix  string  // key prefix, e.g. "mcpgate:"
	now     Clock
	onError func(error)
}

// NewRedis builds a RedisLimiter. rate must be > 0 and burst > 0.
func NewRedis(client redis.Cmdable, rate float64, burst int, keyPrefix string) *RedisLimiter {
	return NewRedisWithClock(client, rate, burst, keyPrefix, time.Now, nil)
}

// NewRedisWithClock builds a RedisLimiter using now as its clock and
// reporting Redis errors to onError (nil means errors are ignored).
// Both hooks exist for tests and for the gateway's metrics.
func NewRedisWithClock(client redis.Cmdable, rate float64, burst int, keyPrefix string, now Clock, onError func(error)) *RedisLimiter {
	return &RedisLimiter{
		client:  client,
		rate:    rate,
		burst:   burst,
		prefix:  keyPrefix,
		now:     now,
		onError: onError,
	}
}

// Allow reports whether key may proceed, consuming one token if so.
// The bucket key is prefix + "ratelimit:" + key.
func (l *RedisLimiter) Allow(key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Idle buckets expire once a full refill is guaranteed: after
	// burst/rate seconds the bucket is full again, plus 1s of slack.
	ttlMs := int64((float64(l.burst)/l.rate + 1) * 1000)
	allowed, err := l.client.Eval(ctx, tokenBucketScript,
		[]string{l.prefix + "ratelimit:" + key},
		l.rate, l.burst, l.now().UnixMilli(), ttlMs).Int64()
	if err != nil {
		if l.onError != nil {
			l.onError(err)
		}
		return true // fail open on Redis errors
	}
	return allowed == 1
}
