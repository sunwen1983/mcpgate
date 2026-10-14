package ratelimit_test

import (
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/ratelimit"
)

func TestBurstThenDeny(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	l := ratelimit.NewWithClock(1, 3, clock) // 1 token/sec, burst 3

	for i := 0; i < 3; i++ {
		if !l.Allow("k") {
			t.Fatalf("request %d: want allow, got deny", i+1)
		}
	}
	if l.Allow("k") {
		t.Error("4th request in the same instant: want deny, got allow")
	}
}

func TestRefillOverTime(t *testing.T) {
	now := time.Now()
	current := now
	clock := func() time.Time { return current }
	l := ratelimit.NewWithClock(1, 2, clock) // 1 token/sec, burst 2

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

func TestPerKeyIsolation(t *testing.T) {
	l := ratelimit.New(1, 1)
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

func TestRefillCapsAtBurst(t *testing.T) {
	now := time.Now()
	current := now
	clock := func() time.Time { return current }
	l := ratelimit.NewWithClock(100, 2, clock)

	l.Allow("k")
	current = now.Add(time.Hour) // huge refill, must cap at burst
	if !l.Allow("k") || !l.Allow("k") {
		t.Error("after long idle: want exactly burst tokens")
	}
	if l.Allow("k") {
		t.Error("burst cap exceeded: want deny")
	}
}
