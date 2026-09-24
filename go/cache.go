package main

import (
	"sync"
	"time"
)

type quotaCacheEntry struct {
	allowed        bool
	remainingQuota int64
	reason         string
	expiresAt      time.Time
}

type quotaCache struct {
	mu         sync.RWMutex
	entries    map[string]quotaCacheEntry
	nowFunc    func() time.Time
	defaultTTL time.Duration
	maxEntries int
}

func newQuotaCache(ttl time.Duration, maxEntries int) *quotaCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &quotaCache{
		entries:    make(map[string]quotaCacheEntry),
		nowFunc:    time.Now,
		defaultTTL: ttl,
		maxEntries: maxEntries,
	}
}

func (c *quotaCache) getNow() time.Time {
	if c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

func (c *quotaCache) Get(key string) (quotaCacheEntry, bool) {
	if c == nil || key == "" {
		return quotaCacheEntry{}, false
	}
	c.mu.RLock()
	entry, exists := c.entries[key]
	c.mu.RUnlock()

	if !exists {
		return quotaCacheEntry{}, false
	}
	if !entry.expiresAt.IsZero() && c.getNow().After(entry.expiresAt) {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return quotaCacheEntry{}, false
	}
	return entry, true
}

func (c *quotaCache) Set(key string, allowed bool, remainingQuota int64, reason string, ttl time.Duration) {
	if c == nil || key == "" {
		return
	}
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	now := c.getNow()
	expiresAt := now.Add(ttl)

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= c.maxEntries {
		c.pruneExpiredLocked(now)
	}

	c.entries[key] = quotaCacheEntry{
		allowed:        allowed,
		remainingQuota: remainingQuota,
		reason:         reason,
		expiresAt:      expiresAt,
	}
}

func (c *quotaCache) Invalidate(key string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *quotaCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]quotaCacheEntry)
}

func (c *quotaCache) pruneExpiredLocked(now time.Time) {
	for k, v := range c.entries {
		if !v.expiresAt.IsZero() && now.After(v.expiresAt) {
			delete(c.entries, k)
		}
	}
}
