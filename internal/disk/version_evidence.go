package disk

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

const versionEvidenceBytes = 8 << 20

// VersionCacheBytes is the per-Computer memory reservation for ciphertext reads
// and authenticated inspection evidence. The caches never grant remote authority.
const VersionCacheBytes = versionReadCacheBytes + versionEvidenceBytes

type versionEvidenceEntry struct {
	inspection blockformat.ObjectInspection
	remote     bool
	bytes      int
}

// Evidence belongs to one LocalVersion's fixed scope, keys and physical lease.
// Inspection evidence is immutable. Every incoming node still checks its
// exact position. Remote evidence additionally requires committed certification
// backed by verified remote bytes and the owner's retained lease closure.
type versionEvidenceCache struct {
	mu      sync.Mutex
	entries map[[32]byte]*list.Element
	recent  list.List
	bytes   int
}

func newVersionEvidenceCache() *versionEvidenceCache {
	return &versionEvidenceCache{entries: make(map[[32]byte]*list.Element)}
}

func evidenceDigest(i blockformat.ObjectInspection) [32]byte {
	if i.Segment != nil {
		return i.Segment.Digest
	}
	return i.Pack.Pages[0].Locator.Pack.Digest
}

func (c *versionEvidenceCache) get(digest [32]byte) (blockformat.ObjectInspection, bool, bool) {
	if c == nil {
		return blockformat.ObjectInspection{}, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[digest]
	if e == nil {
		return blockformat.ObjectInspection{}, false, false
	}
	c.recent.MoveToFront(e)
	v := e.Value.(versionEvidenceEntry)
	return v.inspection, v.remote, true
}

func (c *versionEvidenceCache) put(i blockformat.ObjectInspection, remote bool) {
	if c == nil {
		return
	}
	// JSON carries every array member; four times its length also covers the
	// decoded structs' fixed-width geometry and slice/map bookkeeping.
	raw, err := json.Marshal(i)
	if err != nil {
		return
	}
	cost := 512 + 4*len(raw)
	if cost > versionEvidenceBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	digest := evidenceDigest(i)
	if e := c.entries[digest]; e != nil {
		old := e.Value.(versionEvidenceEntry)
		remote = remote || old.remote
		c.bytes -= old.bytes
		c.recent.Remove(e)
		delete(c.entries, digest)
	}
	for c.bytes+cost > versionEvidenceBytes {
		e := c.recent.Back()
		old := e.Value.(versionEvidenceEntry)
		delete(c.entries, evidenceDigest(old.inspection))
		c.bytes -= old.bytes
		c.recent.Remove(e)
	}
	c.entries[digest] = c.recent.PushFront(versionEvidenceEntry{inspection: i, remote: remote, bytes: cost})
	c.bytes += cost
}

func (c *versionEvidenceCache) pack(ctx context.Context, source blockformat.RangeSource, scope string, keys map[string][]byte, ref blockformat.PackRef, requireRemote bool) (blockformat.PackInspection, error) {
	if err := ctx.Err(); err != nil {
		return blockformat.PackInspection{}, err
	}
	if i, remote, ok := c.get(ref.Digest); ok && (!requireRemote || remote) {
		if i.Pack == nil || len(i.Pack.Pages) == 0 || i.Pack.Pages[0].Locator.Pack != ref {
			return blockformat.PackInspection{}, errors.New("cached pack reference differs")
		}
		return *i.Pack, nil
	}
	inspected, err := blockformat.InspectPack(ctx, source, scope, keys, ref)
	if err == nil {
		c.put(blockformat.ObjectInspection{Pack: &inspected}, requireRemote)
	}
	return inspected, err
}

func (c *versionEvidenceCache) segment(ctx context.Context, source blockformat.RangeSource, scope string, key []byte, ref blockformat.Ref, requireRemote bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i, remote, ok := c.get(ref.Digest); ok && (!requireRemote || remote) {
		if i.Segment == nil || *i.Segment != ref {
			return errors.New("cached segment reference differs")
		}
		return nil
	}
	if err := blockformat.InspectSegment(ctx, source, scope, key, ref); err != nil {
		return err
	}
	c.put(blockformat.ObjectInspection{Segment: &ref}, requireRemote)
	return nil
}
