// Package objectcache retains disposable verified bytes, never publication or
// authorization evidence. Its owner reserves the entire bound before use.
package objectcache

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"golang.org/x/sys/unix"
)

const MaxEntries = 4096

type key struct{ namespace, digest string }
type entry struct {
	descriptor cas.Descriptor
	path       string
	charge     int64
	pins       int
	used       uint64
}

type Stats struct {
	Hits, Misses, Seeds, Evictions uint64
	SeededBytes                    int64
	CapacityBytes, UsedBytes       int64
}

func (c *Cache) Snapshot() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := c.stats
	result.CapacityBytes = c.limit
	result.UsedBytes = c.used
	return result
}

type Cache struct {
	mu          sync.Mutex
	cond        *sync.Cond
	lock        *os.File
	directory   string
	limit, used int64
	clock       uint64
	entries     map[key]*entry
	pending     map[key]bool
	closed      bool
	stats       Stats
}

// Open takes exclusive ownership before removing stale disposable objects.
// The directory and owner.lock must never be replaced while an owner is alive.
func Open(directory string, limit int64) (*Cache, error) {
	if !filepath.IsAbs(directory) || limit < 0 {
		return nil, errors.New("invalid object cache configuration")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("object cache directory must be private")
	}
	fd, err := unix.Open(filepath.Join(directory, "owner.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "object cache owner")
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	objects := filepath.Join(directory, "objects")
	if err := os.RemoveAll(objects); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	if err := os.Mkdir(objects, 0700); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	c := &Cache{lock: lock, directory: objects, limit: limit, entries: make(map[key]*entry), pending: make(map[key]bool)}
	c.cond = sync.NewCond(&c.mu)
	return c, nil
}

// Enable sets the reserved bound once, after stale bytes and VM recovery have
// been removed from the physical capacity calculation.
func (c *Cache) Enable(limit int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.limit != 0 || limit <= 0 {
		return errors.New("cache budget already set or invalid")
	}
	c.limit = limit
	return nil
}

// Seed copies bytes into a private inode. No caller-owned inode or writable
// descriptor is retained. A full or busy cache simply declines the seed.
func (c *Cache) Seed(ctx context.Context, namespace string, d cas.Descriptor, source io.Reader) (bool, error) {
	if c == nil {
		return false, nil
	}
	if err := cas.ValidateDescriptor(d); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	if d.SizeBytes > math.MaxInt64-4095 {
		return false, nil
	}
	charge := (d.SizeBytes + 4095) / 4096 * 4096
	k := key{namespace, d.Digest}
	c.mu.Lock()
	if c.closed || c.pending[k] || c.entries[k] != nil || charge > c.limit {
		c.mu.Unlock()
		return false, nil
	}
	for charge > c.limit-c.used || len(c.entries)+len(c.pending) >= MaxEntries {
		var victim key
		var oldest *entry
		for key, e := range c.entries {
			if e.pins == 0 && (oldest == nil || e.used < oldest.used) {
				victim, oldest = key, e
			}
		}
		if oldest == nil {
			c.mu.Unlock()
			return false, nil
		}
		if err := os.Remove(oldest.path); err != nil {
			c.mu.Unlock()
			return false, err
		}
		delete(c.entries, victim)
		c.stats.Evictions++
		c.used -= oldest.charge
	}
	c.used += charge
	c.pending[k] = true
	c.mu.Unlock()
	var path string
	keep := false
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !keep {
			if path == "" || os.Remove(path) == nil {
				c.used -= charge
			}
		}
		delete(c.pending, k)
		c.cond.Broadcast()
	}()
	f, err := os.CreateTemp(c.directory, "object-")
	if err != nil {
		return false, err
	}
	path = f.Name()
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, source}, d.SizeBytes))
	if copyErr == nil {
		var extra [1]byte
		count, err := contextReader{ctx, source}.Read(extra[:])
		if count != 0 {
			copyErr = cas.ErrDigestMismatch
		} else if err != nil && err != io.EOF {
			copyErr = err
		}
	}
	if copyErr == nil && (n != d.SizeBytes || sha256sum.DigestHash(h) != d.Digest) {
		copyErr = cas.ErrDigestMismatch
	}
	if copyErr == nil {
		copyErr = f.Chmod(0400)
	}
	if err := errors.Join(copyErr, f.Close(), ctx.Err()); err != nil {
		return false, err
	}
	c.mu.Lock()
	if !c.closed {
		c.clock++
		c.entries[k] = &entry{descriptor: d, path: path, charge: charge, used: c.clock}
		keep = true
		c.stats.Seeds++
		c.stats.SeededBytes += d.SizeBytes
	}
	c.mu.Unlock()
	return keep, nil
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

func (c *Cache) open(namespace, digest string) (*reader, cas.Descriptor, bool) {
	if c == nil {
		return nil, cas.Descriptor{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key{namespace, digest}]
	if c.closed || e == nil {
		c.stats.Misses++
		return nil, cas.Descriptor{}, false
	}
	f, err := os.Open(e.path)
	if err != nil {
		return nil, cas.Descriptor{}, false
	}
	c.stats.Hits++
	e.pins++
	c.clock++
	e.used = c.clock
	return &reader{File: f, cache: c, entry: e}, e.descriptor, true
}

type reader struct {
	*os.File
	cache *Cache
	entry *entry
	once  sync.Once
	err   error
}

func (r *reader) Close() error {
	r.once.Do(func() {
		r.err = r.File.Close()
		r.cache.mu.Lock()
		r.entry.pins--
		r.cache.cond.Broadcast()
		r.cache.mu.Unlock()
	})
	return r.err
}

// Close joins fills and open readers before deleting bytes and releasing ownership.
func (c *Cache) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for {
		pins := 0
		for _, e := range c.entries {
			pins += e.pins
		}
		if pins == 0 && len(c.pending) == 0 {
			break
		}
		c.cond.Wait()
	}
	if c.lock == nil {
		return nil
	}
	err := errors.Join(os.RemoveAll(c.directory), c.lock.Close())
	c.lock = nil
	return err
}
