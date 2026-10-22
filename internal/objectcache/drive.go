package objectcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/helmrdotdev/helmr/internal/cas"
)

const driveProbeName = ".helmr-artifact-link-probe"

// EnableDrives qualifies hard links on the actual mounts before any seed. The
// caller supplies the fixed jailer identity; shared inodes retain host ownership.
func (c *Cache) EnableDrives(uid, gid int, destinations ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.drives || len(c.entries) != 0 || len(c.pending) != 0 || gid < 0 || uint64(gid) > uint64(^uint32(0)) || len(destinations) == 0 {
		return errors.New("invalid shared drive cache setup")
	}
	// The process singleton owns these roots. Remove our exact probe name
	// before recovery, including after a crash or a change that disables sharing.
	for _, directory := range destinations {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(directory, driveProbeName)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if uid <= 0 || uid == os.Geteuid() || uint64(uid) > uint64(^uint32(0)) {
		return errors.New("shared drives require a distinct unprivileged jailer owner")
	}
	f, err := os.CreateTemp(c.directory, "link-probe-")
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	defer os.Remove(f.Name())
	for _, directory := range destinations {
		target := filepath.Join(directory, driveProbeName)
		if err := os.Link(f.Name(), target); err != nil {
			return err
		}
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	c.driveGID, c.drives = gid, true
	return nil
}

// DriveLease pins an immutable shared drive. Each consumer must verify its own
// read-only descriptors before linking. VM reservations fund surviving links
// after this pin is released and the cache evicts its name.
type DriveLease struct {
	cache *Cache
	entry *entry
	once  sync.Once
}

func (c *Cache) Borrow(ctx context.Context, namespace string, expected cas.Descriptor) *DriveLease {
	if c == nil || ctx.Err() != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key{namespace, expected.Digest}]
	if c.closed || c.limit == 0 || !c.drives || (namespace != "program" && namespace != "runtime") || e == nil || e.invalid || e.descriptor != expected {
		return nil
	}
	e.pins++
	c.clock++
	e.used = c.clock
	c.stats.Hits++
	return &DriveLease{cache: c, entry: e}
}
func (b *DriveLease) Open() (*os.File, string, int, error) {
	directory, err := os.Open(b.cache.directory)
	return directory, filepath.Base(b.entry.path), b.cache.driveGID, err
}

// Discard makes subsequent reads miss. The entry stays accounted and pinned
// until existing readers release it; normal eviction then removes the name.
func (b *DriveLease) Discard() {
	b.cache.mu.Lock()
	b.entry.invalid = true
	b.cache.mu.Unlock()
}
func (b *DriveLease) Close() error {
	b.once.Do(func() {
		b.cache.mu.Lock()
		b.entry.pins--
		b.cache.cond.Broadcast()
		b.cache.mu.Unlock()
	})
	return nil
}
