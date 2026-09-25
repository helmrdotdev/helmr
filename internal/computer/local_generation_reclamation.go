//go:build linux || darwin

package computer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// LocalCapture retains an exact cut across later local flushes and collection.
// Release only after the publication owner has settled or abandoned the operation
// and joined every consumer. This is local retention, not remote durability.
type LocalCapture struct {
	mu        sync.Mutex
	owner     *LocalGeneration
	root      GenerationRoot
	released  bool
	published bool
}

func (c *LocalCapture) Root() GenerationRoot { return c.root }

func (c *LocalCapture) Publish(ctx context.Context, publisher ContinuationPublication) error {
	c.mu.Lock()
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
	_, err = publishGeneration(ctx, p.store, p.disk.writer.Source, p.disk.writer.Scope, p.disk.writer.Keys, locator, c.root.LogicalBytes, 1<<20, publisher, publisher)
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

// Capture flushes and pins atomically with respect to local collection. A bare
// Flush is for the block protocol's durability acknowledgment, not retained cuts.
func (p *LocalGeneration) Capture(ctx context.Context) (*LocalCapture, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return nil, os.ErrClosed
	}
	p.commit.Lock()
	defer p.commit.Unlock()
	root, err := p.flushLocked(ctx)
	if err != nil {
		return nil, err
	}
	c := &LocalCapture{owner: p, root: root}
	p.captures[c] = root
	return c, nil
}

// Collect removes unreachable ciphertext and bytes covered by the durably adopted
// remote source. It never releases remote ownership or changes the execution base.
// Flush/capture is serialized; ordinary guest reads and writes may continue.
// The finite object budget bounds traversal and directory work. Failure before
// deletion leaves all bytes intact; partial deletion never returns space credit.
func (p *LocalGeneration) Collect(ctx context.Context, maxObjects int) (int64, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return 0, os.ErrClosed
	}
	if maxObjects <= 0 || maxObjects > 1<<20 {
		return 0, errors.New("bounded local collection required")
	}
	// Unreachable collection need not wait for a stalled upload. Reachable
	// eviction is opportunistic: it requires all local-file publishers to join.
	evict := p.publication.TryLock()
	if evict {
		defer p.publication.Unlock()
	}
	p.commit.Lock()
	saved, base := p.head.Saved, p.head.Base
	p.commit.Unlock()
	var remote map[string]bool
	if evict && saved != base {
		var err error
		remote, err = p.remoteBacked(ctx, saved, maxObjects)
		if err != nil {
			return 0, err
		}
	}
	p.commit.Lock()
	defer p.commit.Unlock()
	if p.head.Saved != saved {
		// Another handoff superseded this evidence during remote I/O. Ordinary
		// unreachable collection remains safe; defer eviction to the next pass.
		remote = nil
	}
	// Capture may have succeeded before root-file fsync failed. Both the in-memory
	// tree and the last acknowledged root are live until the next successful flush.
	p.disk.mu.Lock()
	current := p.disk.root
	p.disk.mu.Unlock()
	// Force the latest installed root directory entry durable before deleting
	// any candidate from an earlier ambiguous rename. Until this succeeds, a
	// crash could expose an intermediate root from consecutive failed flushes.
	if err := syncGenerationDirectory(p.directory); err != nil {
		return 0, err
	}
	roots := []GenerationRoot{p.head.Root, p.installedRoot, current}
	for _, root := range p.captures {
		roots = append(roots, root)
	}
	keep, err := p.localReachable(ctx, roots, maxObjects)
	if err != nil {
		return 0, err
	}
	type entry struct {
		path  string
		bytes int64
	}
	var garbage []entry
	var used int64
	objects := filepath.Join(p.directory, "objects")
	err = scanLocalObjects(ctx, objects, maxObjects, func(path string, e os.DirEntry) error {
		info, err := e.Info()
		if err != nil {
			return err
		}
		digest := "sha256:" + e.Name()
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || !sha256sum.ValidDigest(digest) {
			return errors.New("unexpected local collection entry")
		}
		if info.Size() > p.stagedBytes-used {
			return ErrGenerationStagingFull
		}
		used += info.Size()
		if !keep[digest] || remote[digest] {
			garbage = append(garbage, entry{path, info.Size()})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	var removed int64
	for _, e := range garbage {
		if err = ctx.Err(); err != nil {
			return removed, err
		}
		if err = os.Remove(e.path); err != nil {
			return removed, err
		}
		removed += e.bytes
	}
	if len(garbage) > 0 {
		if err = p.step("objects-unlinked"); err != nil {
			return removed, err
		}
	}
	// Also sync a prior partially completed deletion before returning its credit.
	if err = syncGenerationDirectory(filepath.Join(objects, "sha256")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return removed, err
	}
	// This owner serializes every sink write with commit. Recounting also recovers
	// conservative charges from failed I/O and duplicate immutable writes.
	p.disk.writer.Sink.(*reservedGenerationSink).remaining = p.stagedBytes - (used - removed)
	return removed, nil
}

func (p *LocalGeneration) localReachable(ctx context.Context, roots []GenerationRoot, budget int) (map[string]bool, error) {
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
		local, err := generationObjectLocal(ctx, p.store, ref.Digest, ref.Size)
		if err != nil {
			return err
		}
		// Authenticate even remote boundaries. Missing local bytes alone are not
		// evidence that a subtree can be recovered from the retained source.
		inspected, err := blockformat.InspectPack(ctx, p.disk.writer.Source, p.disk.writer.Scope, p.disk.writer.Keys, ref)
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
		locator, err := root.Locator(p.head.Base.LogicalBytes)
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
