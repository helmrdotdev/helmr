//go:build linux || darwin

package disk

import (
	"bytes"
	"context"
	"errors"
	"os"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/ids"
)

// AdoptPrepared installs the granted writer on a pristine private disk. The
// caller must still hold the guest at the pre-mount gate. Source and keys come
// from the real allocation; no preparation credential survives this transition.
func (p *LocalVersion) AdoptPrepared(ctx context.Context, root VersionRoot, activeKey string, keys map[string][]byte, source blockformat.RangeSource) error {
	p.life.Lock()
	defer p.life.Unlock()
	if p.closed {
		return os.ErrClosed
	}
	if source == nil || root != p.head.Base || root != p.head.Root || p.revision != 0 || len(p.captures) != 0 {
		return errors.New("prepared disk source or pristine state differs")
	}
	if _, err := ids.Parse(activeKey); err != nil || len(keys[activeKey]) != 32 {
		return errors.New("prepared disk requires an allocation write key")
	}
	d := p.disk
	d.life.Lock()
	defer d.life.Unlock()
	d.capture.Lock()
	defer d.capture.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.preparedAdopted || d.generation != 0 || len(d.dirty) != 0 || len(d.frozen) != 0 {
		return errors.New("prepared disk was changed or already adopted")
	}
	ownedKeys := make(map[string][]byte, len(keys))
	for id, key := range keys {
		ownedKeys[id] = bytes.Clone(key)
	}
	adopted := false
	defer func() {
		if !adopted {
			for _, key := range ownedKeys {
				clear(key)
			}
		}
	}()
	reads := newVersionRangeCache(guestIORangeSource{source})
	nextSource := localVersionSource{local: p.store, base: guestIORangeSource{source}, reads: reads}
	tree, err := OpenVersion(ctx, nextSource, d.writer.Scope, ownedKeys, root, root.LogicalBytes)
	if err != nil {
		return err
	}
	locator, err := root.Locator(root.LogicalBytes)
	if err != nil {
		return err
	}
	shape, err := blockformat.ReadRoot(ctx, nextSource, d.writer.Scope, ownedKeys, locator)
	if err != nil {
		return err
	}
	budget, err := blockformat.NewCaptureBudget(shape, d.writer.Scope, ownedKeys)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, key := range d.writer.Keys {
		clear(key)
	}
	p.reads.clear()
	p.reads = reads
	d.writer.Source, d.writer.Keys, d.writer.ActiveKey = nextSource, ownedKeys, activeKey
	d.tree, d.budget, d.preparedAdopted = tree, budget, true
	adopted = true
	return nil
}
