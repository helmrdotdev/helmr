package objectcache

import (
	"errors"
	"os"
)

// Resize closes excess fill admission immediately and removes unpinned bytes.
// Remaining bytes include readers and canceled fills that have not joined yet.
// The owner must retain that physical charge and retry; a limit is not evidence
// of released storage. Surviving VM links are funded by their own reservations.
func (c *Cache) Resize(limit int64) (remaining int64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit < 0 || c.closed {
		return c.used, errors.New("invalid object cache resize")
	}
	if limit < c.limit {
		for _, cancel := range c.pending {
			cancel()
		}
	}
	c.limit = limit
	for c.used > limit {
		var victim key
		var oldest *entry
		for key, entry := range c.entries {
			if entry.pins == 0 && (oldest == nil || entry.used < oldest.used) {
				victim, oldest = key, entry
			}
		}
		if oldest == nil {
			break
		}
		if err := os.Remove(oldest.path); err != nil {
			return c.used, err
		}
		delete(c.entries, victim)
		c.used -= oldest.charge
		c.stats.Evictions++
	}
	return c.used, nil
}
