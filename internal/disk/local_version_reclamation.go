//go:build linux || darwin

package disk

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// LocalCapture retains an exact cut across later local flushes and collection.
// Release only after the publication owner has settled or abandoned the operation
// and joined every consumer. This is local retention, not remote durability.
type LocalCapture struct {
	mu        sync.Mutex
	owner     *LocalVersion
	root      VersionRoot
	released  bool
	published bool
	adopted   bool
	changes   VersionChanges
}

func (c *LocalCapture) Root() VersionRoot       { return c.root }
func (c *LocalCapture) Changes() VersionChanges { return c.changes }

func (c *LocalCapture) Publish(ctx context.Context, publisher ContinuationPublication) error {
	lockGuestIO(ctx, &c.mu)
	defer c.mu.Unlock()
	if c.released {
		return os.ErrClosed
	}
	p := c.owner
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return os.ErrClosed
	}
	p.publication.RLock()
	defer p.publication.RUnlock()
	locator, err := c.root.Locator(c.root.LogicalBytes)
	if err != nil {
		return err
	}
	_, err = publishVersionEvidence(ctx, p.store, p.disk.writer.Source, p.disk.writer.Scope, p.disk.writer.Keys, locator, c.root.LogicalBytes, 1<<20, publisher, publisher, p.evidence)
	if err == nil {
		c.published = true
	}
	return err
}

func (c *LocalCapture) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		return
	}
	c.owner.commit.Lock()
	delete(c.owner.captures, c)
	c.owner.commit.Unlock()
	c.released = true
}

// Capture cuts between complete NBD requests and pins atomically with respect to
// local collection. Encoding and local persistence do not hold the request gate.
// The guest must acknowledge its filesystem flush before this call when the cut
// certifies guest writes. A host-local cut alone does not establish that boundary.
func (p *LocalVersion) Capture(ctx context.Context) (*LocalCapture, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return nil, os.ErrClosed
	}
	p.storage.RLock()
	defer p.storage.RUnlock()
	// Requests can flush under buffer pressure. Never wait for commit while
	// holding request: an earlier encoder must not stall otherwise runnable I/O.
	// Acquire both only when commit is immediately available; otherwise join the
	// previous commit outside the request gate and retry the boundary.
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lockGuestIO(ctx, &p.request)
		if p.commit.TryLock() {
			break
		}
		p.request.Unlock()
		lockGuestIO(ctx, &p.commit)
		p.commit.Unlock()
	}
	defer p.commit.Unlock()
	// The request and commit gates exclude mutations until captureBoundary swaps
	// the dirty map. Only this publication-owner cut transfers autosave age;
	// ordinary guest flushes must leave it observable.
	lockGuestIO(ctx, &p.disk.mu)
	changes := VersionChanges{Generation: p.disk.generation, ChangedAt: p.disk.changedAt, LastChangedAt: p.disk.lastChangedAt}
	p.disk.changedAt, p.disk.lastChangedAt = time.Time{}, time.Time{}
	p.disk.mu.Unlock()
	root, err := p.flushBoundaryLocked(ctx, p.request.Unlock)
	if err != nil {
		// Persistence can fail after newer requests resume. Restore the whole
		// unpublished interval without replacing those newer mutations.
		p.disk.mu.Lock()
		if !changes.ChangedAt.IsZero() && (p.disk.changedAt.IsZero() || changes.ChangedAt.Before(p.disk.changedAt)) {
			p.disk.changedAt = changes.ChangedAt
		}
		if changes.LastChangedAt.After(p.disk.lastChangedAt) {
			p.disk.lastChangedAt = changes.LastChangedAt
		}
		p.disk.mu.Unlock()
		return nil, err
	}
	c := &LocalCapture{owner: p, root: root, changes: changes}
	p.captures[c] = root
	return c, nil
}

// Collect scans immutable candidates before taking a root snapshot, traverses
// outside commit, then rechecks the root revision before each bounded deletion
// batch. A capture either completes before that snapshot or invalidates it.
// Active publishers prevent reachable eviction, but not unrelated garbage.
func (p *LocalVersion) Collect(ctx context.Context, maxObjects int) (int64, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return 0, os.ErrClosed
	}
	if maxObjects <= 0 || maxObjects > 1<<20 {
		return 0, errors.New("bounded local collection required")
	}
	if !p.collection.TryLock() {
		return 0, nil
	}
	defer p.collection.Unlock()
	type entry struct {
		path, digest string
		bytes        int64
	}
	var candidates []entry
	var used int64
	sink := p.disk.writer.Sink.(*reservedVersionSink)
	// Start the census only between encoders. Charges are reserved before local
	// I/O, so a generation sampled during an encoder can miss retained bytes.
	if !p.commit.TryLock() {
		return 0, nil
	}
	charges := sink.chargeGeneration()
	p.commit.Unlock()
	objects := filepath.Join(p.directory, "objects")
	// Only this pre-snapshot set can be deleted. Files produced by a later
	// capture are never mistaken for unreachable objects from this pass.
	err := scanLocalObjects(ctx, objects, maxObjects, func(path string, e os.DirEntry) error {
		info, err := e.Info()
		if err != nil {
			return err
		}
		digest := "sha256:" + e.Name()
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || !sha256sum.ValidDigest(digest) {
			return errors.New("unexpected local collection entry")
		}
		if info.Size() > p.stagedBytes-used {
			return ErrVersionStagingFull
		}
		used += info.Size()
		candidates = append(candidates, entry{path, digest, info.Size()})
		return nil
	})
	lockGuestIO(ctx, &p.commit)
	if sink.chargeGeneration() != charges {
		p.commit.Unlock()
		return 0, nil
	}
	if err != nil {
		p.commit.Unlock()
		return 0, err
	}
	saved, base, revision := p.head.Saved, p.head.Base, p.revision
	lockGuestIO(ctx, &p.disk.mu)
	current := p.disk.root
	p.disk.mu.Unlock()
	roots := []VersionRoot{p.head.Root, p.installedRoot, current}
	for _, root := range p.captures {
		roots = append(roots, root)
	}
	backing := make(map[string]bool, len(p.verifiedBacking))
	for digest := range p.verifiedBacking {
		backing[digest] = true
	}
	p.commit.Unlock()
	keep, err := p.localReachable(ctx, roots, maxObjects)
	if err != nil {
		return 0, err
	}
	// Certification and the saved root retain this remote closure for the lease.
	// Evidence may be reused; every incoming link still checks its position.
	if saved != base {
		remote, err := p.remoteBacked(ctx, saved, maxObjects)
		if err != nil {
			return 0, err
		}
		for digest := range remote {
			backing[digest] = true
		}
	}
	var garbage []entry
	for _, e := range candidates {
		if !keep[e.digest] || backing[e.digest] {
			garbage = append(garbage, e)
		}
	}
	var removed int64
	// Even an empty pass can recover conservative charges after failed/duplicate
	// immutable writes, provided no sink write raced the directory census.
	for len(garbage) > 0 || removed == 0 {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		lockGuestIO(ctx, &p.commit)
		if p.revision != revision || !p.storage.TryLock() {
			p.commit.Unlock()
			return removed, nil
		}
		// Never wait for an encoder while holding commit. The storage gate protects
		// the unchanged roots and active read snapshots throughout physical deletion.
		p.commit.Unlock()
		evict := p.publication.TryLock()
		batch := garbage[:min(64, len(garbage))]
		var unlinked int64
		var deleted []string
		err = func() error {
			// Complete an ambiguous root rename before removing its prior recovery set.
			if err := syncVersionDirectory(p.directory); err != nil {
				return err
			}
			for _, e := range batch {
				if keep[e.digest] && !evict {
					continue
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := os.Remove(e.path); err != nil {
					return err
				}
				unlinked += e.bytes
				deleted = append(deleted, e.digest)
			}
			if unlinked > 0 {
				if err := p.step("objects-unlinked"); err != nil {
					return err
				}
			}
			if err := syncVersionDirectory(filepath.Join(objects, "sha256")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			sink.creditDeleted(unlinked)
			return nil
		}()
		if evict {
			p.publication.Unlock()
		}
		p.commit.Lock()
		for _, digest := range deleted {
			delete(p.verifiedBacking, digest)
		}
		p.commit.Unlock()
		removed += unlinked
		if err == nil && len(garbage) <= len(batch) {
			sink.recount(charges, p.stagedBytes-(used-removed))
		}
		p.storage.Unlock()
		if err != nil {
			return removed, err
		}
		garbage = garbage[len(batch):]
		if len(garbage) == 0 {
			return removed, nil
		}
	}
	return removed, nil
}

func (p *LocalVersion) localReachable(ctx context.Context, roots []VersionRoot, budget int) (map[string]bool, error) {
	keep := make(map[string]bool)
	visited := make(map[blockformat.PackRef]bool)
	charge := func(digest string) error {
		if !keep[digest] {
			if len(keep) >= budget {
				return errors.New("local collection graph budget exceeded")
			}
			keep[digest] = true
		}
		return ctx.Err()
	}
	var visit func(blockformat.PackRef, *blockformat.NodeReference, *blockformat.Locator, int64) error
	visit = func(ref blockformat.PackRef, link *blockformat.NodeReference, root *blockformat.Locator, capacity int64) error {
		digest := sha256sum.FormatDigest(ref.Digest[:])
		if err := charge(digest); err != nil {
			return err
		}
		local, err := versionObjectLocal(ctx, p.store, ref.Digest, ref.Size)
		if err != nil {
			return err
		}
		// Authenticate even remote boundaries. Missing local bytes alone are not
		// evidence that a subtree can be recovered from the retained source.
		inspected, err := p.evidence.pack(ctx, p.disk.writer.Source, p.disk.writer.Scope, p.disk.writer.Keys, ref, false)
		if err != nil {
			return err
		}
		if link != nil {
			err = inspected.CheckNode(*link)
		} else {
			err = inspected.CheckRoot(*root, capacity)
		}
		if err != nil {
			return err
		}
		if !local || visited[ref] {
			return nil
		}
		visited[ref] = true
		for _, page := range inspected.Pages {
			for _, segment := range page.Segments {
				if err = charge(sha256sum.FormatDigest(segment.Digest[:])); err != nil {
					return err
				}
			}
			for _, child := range page.Children {
				if child.Locator.Pack.Rank >= ref.Rank {
					return errors.New("invalid collection dependency rank")
				}
				if err = visit(child.Locator.Pack, &child, nil, capacity); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, root := range roots {
		locator, err := root.Locator(root.LogicalBytes)
		if err != nil {
			return nil, err
		}
		if err = visit(locator.Pack, nil, &locator, root.LogicalBytes); err != nil {
			return nil, err
		}
	}
	return keep, nil
}

// Read directory entries in finite batches, without WalkDir/ReadDir(path)'s
// whole-directory allocation. Immutable local CAS has exactly one hash directory.
func scanLocalObjects(ctx context.Context, directory string, budget int, visit func(string, os.DirEntry) error) error {
	scan := func(path string, fn func(os.DirEntry) error) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			if err = ctx.Err(); err != nil {
				return err
			}
			entries, readErr := f.ReadDir(min(128, budget+1))
			for _, e := range entries {
				budget--
				if budget < 0 {
					return errors.New("local collection directory budget exceeded")
				}
				if err = fn(e); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	hasObjects := false
	if err := scan(directory, func(e os.DirEntry) error {
		if e.Name() != "sha256" || !e.IsDir() {
			return errors.New("unexpected local object directory")
		}
		hasObjects = true
		return nil
	}); err != nil {
		return err
	}
	if !hasObjects {
		return nil
	}
	return scan(filepath.Join(directory, "sha256"), func(e os.DirEntry) error {
		return visit(filepath.Join(directory, "sha256", e.Name()), e)
	})
}

// PinCommitted retains the last acknowledged local root without guest flush,
// request drainage or another capture. It certifies no completed Turn prefix.
func (p *LocalVersion) PinCommitted(ctx context.Context) (*LocalCapture, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return nil, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lockGuestIO(ctx, &p.commit)
	defer p.commit.Unlock()
	c := &LocalCapture{owner: p, root: p.head.Root, changes: VersionChanges{Generation: p.headGeneration}}
	p.captures[c] = c.root
	return c, nil
}
