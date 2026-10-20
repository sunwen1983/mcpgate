// Package cache is a small thread-safe TTL cache for backend responses.
//
// Keys are opaque strings chosen by the caller (the gateway builds them from
// backend + method + canonical params). Entries expire lazily on read; there
// is no background janitor.
package cache

import (
	"sync"
	"time"
)

// entry is one cached value with its expiry.
type entry struct {
	value     []byte
	expiresAt time.Time
}

// Cache is a TTL cache safe for concurrent use.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]entry
	now     func() time.Time
}

// New builds an empty Cache using the wall clock.
func New() *Cache {
	return &Cache{entries: map[string]entry{}, now: time.Now}
}

// NewWithClock builds a Cache using now as its clock (for tests).
func NewWithClock(now func() time.Time) *Cache {
	return &Cache{entries: map[string]entry{}, now: now}
}

// Get returns the cached value for key, or nil, false on a miss or expiry.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if c.now().After(e.expiresAt) {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return nil, false
	}
	// Return a copy so callers cannot mutate the cached bytes.
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, true
}

// Set stores value under key for ttl. A ttl <= 0 disables caching for the
// entry (nothing is stored).
func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	stored := make([]byte, len(value))
	copy(stored, value)
	c.mu.Lock()
	c.entries[key] = entry{value: stored, expiresAt: c.now().Add(ttl)}
	c.mu.Unlock()
}

// Delete removes key from the cache.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// Clear removes every entry from the cache and returns how many were
// removed. Used by the admin cache-invalidation endpoint.
func (c *Cache) Clear() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.entries)
	c.entries = map[string]entry{}
	return n
}

// Len returns the number of stored entries, including expired-but-unread ones.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
