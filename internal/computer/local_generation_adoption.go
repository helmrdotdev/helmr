//go:build linux || darwin

package computer

import (
	"context"
	"errors"
	"os"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// Adopt records a published cut as a remote read source, without rolling back
// newer local writes or changing the execution base. The caller must first
// confirm durable publication of this exact root and retain its remote closure
// and keys for this local owner's lifetime. Upload success alone is insufficient.
// The caller serializes adoption in publication order. Only after success may it
// acknowledge the handoff; an error (including an ambiguous fsync) retains the
// pending publication. Collection is separate and can be retried independently.
func (c *LocalCapture) Adopt(ctx context.Context, maxObjects int) error {
	c.mu.Lock()
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
	// Remote verification may be slow. The pinned immutable capture and life
	// lock protect its keys/source; do not block guest flushes during this I/O.
	if _, err := p.remoteBacked(ctx, c.root, maxObjects); err != nil {
		return err
	}
	p.commit.Lock()
	defer p.commit.Unlock()
	head := p.head
	head.Saved = c.root
	if err := p.persist(ctx, head); err != nil {
		return err
	}
	p.head = head
	return nil
}

// remoteBacked authenticates remote copies before permitting local eviction.
// Traverse only locally present packs; an absent pack is an already retained
// remote boundary. Inspect every incoming position even for a visited pack.
// The result is bounded, temporary evidence, never a second durable object index.
func (p *LocalGeneration) remoteBacked(ctx context.Context, root GenerationRoot, budget int) (map[string]bool, error) {
	if budget <= 0 || budget > 1<<20 {
		return nil, errors.New("bounded source adoption required")
	}
	remote := p.disk.writer.Source.(localGenerationSource).base
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
		inspected, err := blockformat.InspectPack(ctx, remote, p.disk.writer.Scope, p.disk.writer.Keys, ref)
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
		local, err := generationObjectLocal(ctx, p.store, ref.Digest, ref.Size)
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
				local, err := generationObjectLocal(ctx, p.store, segment.Digest, segment.Size)
				if err != nil {
					return err
				}
				if local {
					if err = blockformat.InspectSegment(ctx, remote, p.disk.writer.Scope, p.disk.writer.Keys[segment.Key], segment); err != nil {
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

// Collect reclaims local staging after the Runtime settles publication. It may
// run after Release; the Runtime must retain the generation until it returns.
func (c *LocalCapture) Collect(ctx context.Context, maxObjects int) (int64, error) {
	return c.owner.Collect(ctx, maxObjects)
}
