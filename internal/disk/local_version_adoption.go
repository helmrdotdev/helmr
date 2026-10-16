//go:build linux || darwin

package disk

import (
	"context"
	"errors"
	"os"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// Adopt records a published cut as a remote read source, without rolling back
// newer local writes or changing the execution base. Publish must have completed
// authenticated object certification for this exact capture, and the caller must
// confirm its durable recovery-head publication. The caller retains that remote
// closure and its keys until a later adopted cut passes ReadyForRetirement, or
// the owner is physically fenced. Upload success alone is insufficient.
// The successful publication supplies immutable closure evidence; adoption does
// not repeat its remote traversal or perform a fresh integrity audit. Collection
// separately verifies backing before reachable local eviction.
// The caller serializes adoption in publication order. Only after success may it
// acknowledge the handoff; an error (including an ambiguous fsync) retains the
// pending publication. Collection is separate and can be retried independently.
func (c *LocalCapture) Adopt(ctx context.Context) error {
	lockGuestIO(ctx, &c.mu)
	defer c.mu.Unlock()
	if c.released {
		return os.ErrClosed
	}
	if !c.published {
		return errors.New("capture has not completed publication")
	}
	p := c.owner
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return os.ErrClosed
	}
	p.publication.RLock()
	defer p.publication.RUnlock()
	lockGuestIO(ctx, &p.commit)
	defer p.commit.Unlock()
	head := p.head
	head.Saved = c.root
	if err := p.persist(ctx, head); err != nil {
		return err
	}
	p.head = head
	c.adopted = true
	return nil
}

// remoteBacked authenticates remote copies before permitting local eviction.
// Traverse only locally present packs; an absent pack is an already retained
// remote boundary. Inspect every incoming position even for a visited pack.
// The result is bounded, temporary evidence, never a second durable object index.
func (p *LocalVersion) remoteBacked(ctx context.Context, root VersionRoot, budget int) (map[string]bool, error) {
	if budget <= 0 || budget > 1<<20 {
		return nil, errors.New("bounded source adoption required")
	}
	remote := p.disk.writer.Source.(localVersionSource).base
	backed := make(map[string]bool)
	visited := make(map[blockformat.PackRef]bool)
	charge := func(digest string) error {
		if !backed[digest] {
			if len(backed) >= budget {
				return errors.New("source adoption graph budget exceeded")
			}
			backed[digest] = true
		}
		return ctx.Err()
	}
	locator, err := root.Locator(root.LogicalBytes)
	if err != nil {
		return nil, err
	}
	var visit func(blockformat.PackRef, *blockformat.NodeReference) error
	visit = func(ref blockformat.PackRef, link *blockformat.NodeReference) error {
		if err := charge(sha256sum.FormatDigest(ref.Digest[:])); err != nil {
			return err
		}
		inspected, err := p.evidence.pack(ctx, remote, p.disk.writer.Scope, p.disk.writer.Keys, ref, true)
		if err != nil {
			return err
		}
		if link == nil {
			err = inspected.CheckRoot(locator, root.LogicalBytes)
		} else {
			err = inspected.CheckNode(*link)
		}
		if err != nil {
			return err
		}
		local, err := versionObjectLocal(ctx, p.store, ref.Digest, ref.Size)
		if err != nil || !local || visited[ref] {
			return err
		}
		visited[ref] = true
		for _, page := range inspected.Pages {
			for _, segment := range page.Segments {
				digest := sha256sum.FormatDigest(segment.Digest[:])
				if backed[digest] {
					continue
				}
				if err = charge(digest); err != nil {
					return err
				}
				local, err := versionObjectLocal(ctx, p.store, segment.Digest, segment.Size)
				if err != nil {
					return err
				}
				if local {
					if err = p.evidence.segment(ctx, remote, p.disk.writer.Scope, p.disk.writer.Keys[segment.Key], segment, true); err != nil {
						return err
					}
				}
			}
			for _, child := range page.Children {
				if child.Locator.Pack.Rank >= ref.Rank {
					return errors.New("invalid adoption dependency rank")
				}
				if err = visit(child.Locator.Pack, &child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err = visit(locator.Pack, nil); err != nil {
		return nil, err
	}
	return backed, nil
}

// Collect reclaims local staging after the Instance settles publication. It may
// run after Release; the Instance must retain the version until it returns.
func (c *LocalCapture) Collect(ctx context.Context, maxObjects int) (int64, error) {
	// Keep local ciphertext as a cache while the reserved staging envelope has
	// room. Pressure recovery owns collection when an admission cannot fit.
	if !c.owner.disk.pressureActive.Load() {
		return 0, ctx.Err()
	}
	return c.owner.Collect(ctx, maxObjects)
}

// BackRemotely permits eviction of authenticated remotely retained bytes without
// advancing the published recovery head. The publication owner must keep the
// original requested save and its registered object/key closure retained until
// a later adopted cut passes readiness, or writer fencing. No Capture/Publish
// receipt or Turn coverage is implied.
func (c *LocalCapture) BackRemotely(ctx context.Context, maxObjects int) error {
	lockGuestIO(ctx, &c.mu)
	defer c.mu.Unlock()
	if c.released {
		return os.ErrClosed
	}
	if !c.published {
		return errors.New("capture has not completed object publication")
	}
	p := c.owner
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return os.ErrClosed
	}
	p.publication.RLock()
	defer p.publication.RUnlock()
	backed, err := p.remoteBacked(ctx, c.root, maxObjects)
	if err != nil {
		return err
	}
	lockGuestIO(ctx, &p.commit)
	defer p.commit.Unlock()
	for digest := range backed {
		p.verifiedBacking[digest] = true
	}
	return nil
}

// ReadyForRetirement joins existing readers, encoders, graph traversals and
// publishers without waiting. No remote I/O occurs under these gates. A busy
// reader or an older retained capture defers progress; this is not a time bound.
// Only a fresh successful Adopt can authorize an announcement after restart.
func (c *LocalCapture) ReadyForRetirement() bool {
	if !c.mu.TryLock() {
		return false
	}
	defer c.mu.Unlock()
	if !c.adopted {
		return false
	}
	p := c.owner
	if !p.life.TryRLock() {
		return false
	}
	defer p.life.RUnlock()
	if p.closed || !p.collection.TryLock() {
		return false
	}
	defer p.collection.Unlock()
	if !p.publication.TryLock() {
		return false
	}
	defer p.publication.Unlock()
	if !p.commit.TryLock() {
		return false
	}
	defer p.commit.Unlock()
	if !p.storage.TryLock() {
		return false
	}
	defer p.storage.Unlock()
	if p.head.Saved != c.root || p.installedRoot != p.head.Root {
		return false
	}
	for capture := range p.captures {
		if capture.changes.Generation < c.changes.Generation {
			return false
		}
	}
	return true
}
