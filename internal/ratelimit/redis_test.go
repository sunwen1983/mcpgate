package ratelimit_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sunwen1983/mcpgate/internal/ratelimit"
)

// newTestRedis starts an in-process Redis (miniredis) and returns a
// client for it. The server speaks real RESP, so the Lua token-bucket
// script runs exactly as it would against a real Redis.
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

func TestRedisBurstThenDenySharedAcrossInstances(t *testing.T) {
	_, client := newTestRedis(t)
	// Two limiter instances sharing one store must share one budget:
	// that is the whole point of the Redis backend.
	a := ratelimit.NewRedis(client, 1, 3, "mcpgate:")
	b := ratelimit.NewRedis(client, 1, 3, "mcpgate:")

	for i := 0; i < 3; i++ {
		lim := a
		if i%2 == 1 {
			lim = b
		}
		if !lim.Allow("k") {
			t.Fatalf("request %d: want allow, got deny", i+1)
		}
	}
	if a.Allow("k") || b.Allow("k") {
		t.Error("4th request across instances: want deny, got allow")
	}
}

func TestRedisRefillOverTime(t *testing.T) {
	_, client := newTestRedis(t)
	now := time.Now()
	current := now
	clock := func() time.Time { return current }
	l := ratelimit.NewRedisWithClock(client, 1, 2, "mcpgate:", clock, nil)

	l.Allow("k")
	l.Allow("k")
	if l.Allow("k") {
		t.Fatal("bucket exhausted: want deny")
	}
	current = now.Add(1500 * time.Millisecond) // +1.5 tokens
	if !l.Allow("k") {
		t.Error("after 1.5s refill: want allow")
	}
	if l.Allow("k") {
		t.Error("only 1.5 tokens refilled: second request should deny")
	}
}

func TestRedisPerKeyIsolation(t *testing.T) {
	_, client := newTestRedis(t)
	l := ratelimit.NewRedis(client, 1, 1, "mcpgate:")
	if !l.Allow("a") {
		t.Fatal("key a: want allow")
	}
	if !l.Allow("b") {
		t.Error("key b has its own bucket: want allow")
	}
	if l.Allow("a") {
		t.Error("key a exhausted: want deny")
	}
}

func TestRedisKeyPrefixIsolation(t *testing.T) {
	_, client := newTestRedis(t)
	// Two gateways with different prefixes sharing one Redis must not
	// share budgets — the prefix is the namespace boundary.
	a := ratelimit.NewRedis(client, 1, 1, "gw-a:")
	b := ratelimit.NewRedis(client, 1, 1, "gw-b:")
	if !a.Allow("k") {
		t.Fatal("gw-a key k: want allow")
	}
	if !b.Allow("k") {
		t.Error("gw-b key k has its own bucket: want allow")
	}
}

func TestRedisFailOpenOnError(t *testing.T) {
	s, client := newTestRedis(t)
	var gotErr error
	l := ratelimit.NewRedisWithClock(client, 1, 1, "mcpgate:", time.Now, func(err error) {
		gotErr = err
	})
	if !l.Allow("k") {
		t.Fatal("first request: want allow")
	}
	// Kill Redis: the next check must fail open (allow) and report
	// the error, not deny traffic or panic.
	s.Close()
	if !l.Allow("k") {
		t.Error("Redis down: want fail-open allow, got deny")
	}
	if gotErr == nil {
		t.Error("Redis down: want onError called, got nil")
	}
}

func TestDial(t *testing.T) {
	s, _ := newTestRedis(t)
	client, err := ratelimit.Dial(s.Addr(), "", 0)
	if err != nil {
		t.Fatalf("Dial live Redis: %v", err)
	}
	_ = client.Close()

	// Nothing listens here: Dial must fail fast instead of hanging.
	if _, err := ratelimit.Dial("127.0.0.1:1", "", 0); err == nil {
		t.Error("Dial closed port: want error, got nil")
	}
}
