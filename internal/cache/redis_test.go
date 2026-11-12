package cache_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sunwen1983/mcpgate/internal/cache"
)

// newTestRedis starts an in-process Redis (miniredis) and returns a
// client for it.
func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(s.Close)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return s, client
}

func TestRedisRoundTripSharedAcrossInstances(t *testing.T) {
	_, client := newTestRedis(t)
	// Two cache instances sharing one store must share entries: that
	// is the whole point of the Redis backend.
	a := cache.NewRedis(client, "mcpgate:")
	b := cache.NewRedis(client, "mcpgate:")

	a.Set("k", []byte(`{"ok":true}`), time.Minute)
	got, hit := b.Get("k")
	if !hit {
		t.Fatal("other instance: want hit, got miss")
	}
	if !bytes.Equal(got, []byte(`{"ok":true}`)) {
		t.Errorf("got %q, want %q", got, `{"ok":true}`)
	}
	if _, hit := a.Get("missing"); hit {
		t.Error("unknown key: want miss, got hit")
	}
}

func TestRedisExpiry(t *testing.T) {
	s, client := newTestRedis(t)
	c := cache.NewRedis(client, "mcpgate:")
	c.Set("k", []byte("v"), 100*time.Millisecond)
	if _, hit := c.Get("k"); !hit {
		t.Fatal("before expiry: want hit")
	}
	s.FastForward(200 * time.Millisecond)
	if _, hit := c.Get("k"); hit {
		t.Error("after expiry: want miss, got hit")
	}
}

func TestRedisZeroTTLStoresNothing(t *testing.T) {
	_, client := newTestRedis(t)
	c := cache.NewRedis(client, "mcpgate:")
	c.Set("k", []byte("v"), 0)
	if _, hit := c.Get("k"); hit {
		t.Error("ttl 0: want miss, got hit")
	}
	c.Set("k", []byte("v"), -time.Second)
	if _, hit := c.Get("k"); hit {
		t.Error("negative ttl: want miss, got hit")
	}
}

func TestRedisDelete(t *testing.T) {
	_, client := newTestRedis(t)
	c := cache.NewRedis(client, "mcpgate:")
	c.Set("k", []byte("v"), time.Minute)
	c.Delete("k")
	if _, hit := c.Get("k"); hit {
		t.Error("after Delete: want miss, got hit")
	}
	// Deleting a missing key is a no-op, not an error.
	c.Delete("missing")
}

func TestRedisClearOnlyTouchesCacheKeys(t *testing.T) {
	s, client := newTestRedis(t)
	c := cache.NewRedis(client, "mcpgate:")
	c.Set("a", []byte("1"), time.Minute)
	c.Set("b", []byte("2"), time.Minute)
	// A rate-limiter bucket and an unrelated key share the same Redis
	// and must survive a cache clear.
	if err := client.Set(context.Background(), "mcpgate:ratelimit:k", "x", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(context.Background(), "other-app:k", "x", 0).Err(); err != nil {
		t.Fatal(err)
	}

	if n := c.Clear(); n != 2 {
		t.Errorf("Clear() = %d, want 2", n)
	}
	if _, hit := c.Get("a"); hit {
		t.Error("after Clear: key a should be gone")
	}
	if _, hit := c.Get("b"); hit {
		t.Error("after Clear: key b should be gone")
	}
	if !s.Exists("mcpgate:ratelimit:k") {
		t.Error("after Clear: rate-limiter key should survive")
	}
	if !s.Exists("other-app:k") {
		t.Error("after Clear: unrelated key should survive")
	}
	// Clearing an empty cache reports zero.
	if n := c.Clear(); n != 0 {
		t.Errorf("second Clear() = %d, want 0", n)
	}
}

func TestRedisKeyPrefixIsolation(t *testing.T) {
	_, client := newTestRedis(t)
	// Two gateways with different prefixes sharing one Redis must not
	// share cached responses — the prefix is the namespace boundary.
	a := cache.NewRedis(client, "gw-a:")
	b := cache.NewRedis(client, "gw-b:")
	a.Set("k", []byte("v"), time.Minute)
	if _, hit := b.Get("k"); hit {
		t.Error("gw-b: want miss, got hit on gw-a's entry")
	}
	if n := b.Clear(); n != 0 {
		t.Errorf("gw-b Clear() = %d, want 0", n)
	}
	if _, hit := a.Get("k"); !hit {
		t.Error("gw-b Clear must not remove gw-a's entry")
	}
}

func TestRedisFailOpenOnError(t *testing.T) {
	s, client := newTestRedis(t)
	var errs int
	c := cache.NewRedisWithOnError(client, "mcpgate:", func(err error) { errs++ })
	c.Set("k", []byte("v"), time.Minute)
	// Kill Redis: reads must miss (fail open) and writes must drop,
	// each reporting the error instead of panicking.
	s.Close()
	if _, hit := c.Get("k"); hit {
		t.Error("Redis down: Get want miss, got hit")
	}
	c.Set("k2", []byte("v"), time.Minute)
	c.Delete("k")
	if n := c.Clear(); n != 0 {
		t.Errorf("Redis down: Clear() = %d, want 0", n)
	}
	if errs == 0 {
		t.Error("Redis down: want onError called, got no calls")
	}
}
