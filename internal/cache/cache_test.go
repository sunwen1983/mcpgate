package cache_test

import (
	"testing"
	"time"

	"github.com/sunwen1983/mcpgate/internal/cache"
)

func TestSetGetRoundTrip(t *testing.T) {
	c := cache.New()
	c.Set("k", []byte("v"), time.Minute)
	got, ok := c.Get("k")
	if !ok || string(got) != "v" {
		t.Errorf("Get = (%q, %v), want (\"v\", true)", got, ok)
	}
}

func TestMissOnUnknownKey(t *testing.T) {
	c := cache.New()
	if _, ok := c.Get("nope"); ok {
		t.Error("Get(unknown) = hit, want miss")
	}
}

func TestExpiry(t *testing.T) {
	now := time.Now()
	current := now
	c := cache.NewWithClock(func() time.Time { return current })
	c.Set("k", []byte("v"), time.Minute)
	current = now.Add(61 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Error("Get after TTL: want miss")
	}
}

func TestZeroTTlDisablesCaching(t *testing.T) {
	c := cache.New()
	c.Set("k", []byte("v"), 0)
	if _, ok := c.Get("k"); ok {
		t.Error("Set with ttl 0: want nothing stored")
	}
}

func TestStoredValueIsCopied(t *testing.T) {
	c := cache.New()
	original := []byte("abc")
	c.Set("k", original, time.Minute)
	original[0] = 'X'
	got, _ := c.Get("k")
	if string(got) != "abc" {
		t.Errorf("mutating the input changed the cache: got %q", got)
	}
	got[0] = 'Y'
	got2, _ := c.Get("k")
	if string(got2) != "abc" {
		t.Errorf("mutating the result changed the cache: got %q", got2)
	}
}

func TestDelete(t *testing.T) {
	c := cache.New()
	c.Set("k", []byte("v"), time.Minute)
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Error("Get after Delete: want miss")
	}
}

func TestClear(t *testing.T) {
	c := cache.New()
	c.Set("a", []byte("1"), time.Minute)
	c.Set("b", []byte("2"), time.Minute)
	if n := c.Clear(); n != 2 {
		t.Errorf("Clear = %d, want 2", n)
	}
	if c.Len() != 0 {
		t.Errorf("Len after Clear = %d, want 0", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Error("Get after Clear: want miss")
	}
	if n := c.Clear(); n != 0 {
		t.Errorf("Clear on empty cache = %d, want 0", n)
	}
}
