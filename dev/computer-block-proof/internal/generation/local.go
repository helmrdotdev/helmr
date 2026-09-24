package generation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

// Local is a development-only single-owner directory, outside customer control.
// Its caller owns exclusivity and a durably created parent directory. It has no
// eviction, concurrent writers, remote preservation or production admission.
type Local struct {
	dir   string
	after func(string) error
}

func OpenLocal(dir string) (*Local, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, errors.New("local directory required")
	}
	if err = os.Mkdir(filepath.Join(dir, "objects"), 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err = syncDirectory(dir); err != nil {
		return nil, err
	}
	return &Local{dir: dir}, nil
}
func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (l *Local) boundary(name string) error {
	if l.after != nil {
		return l.after(name)
	}
	return nil
}
func readBounded(path string, size int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > size {
		return nil, errors.New("invalid local file size or type")
	}
	return io.ReadAll(io.LimitReader(f, size+1))
}
func (l *Local) objectPath(digest [32]byte) string {
	return filepath.Join(l.dir, "objects", hex.EncodeToString(digest[:]))
}
func (l *Local) install(r blockformat.Ref, raw []byte) error {
	path := l.objectPath(r.Digest)
	existing, e := readBounded(path, r.Size)
	if e == nil {
		if int64(len(existing)) != r.Size || sha256.Sum256(existing) != r.Digest {
			return errors.New("existing local object corrupt")
		}
		// An object may have survived an interrupted earlier commit: sync it again.
		f, e := os.OpenFile(path, os.O_RDWR, 0)
		if e != nil {
			return e
		}
		e = f.Sync()
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		return l.boundary("object-synced")
	}
	if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	f, e := os.CreateTemp(filepath.Join(l.dir, "objects"), "pending-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, e = f.Write(raw); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = l.boundary("object-synced"); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	return l.boundary("object-installed")
}

// Commit orders immutable file sync, object directory sync, root file sync,
// atomic root replacement and root directory sync. After root replacement an
// error has an uncertain outcome: Reopen may select the new valid generation.
// Old objects are never deleted. No success is returned before the final sync.
func (l *Local) Commit(c *Codec, data, packs *Store, root blockformat.Locator, maxObjects, maxBytes int64) error {
	if _, e := Certify(c, data, packs, root, maxObjects, maxBytes); e != nil {
		return e
	}
	refs := map[blockformat.Ref]*Store{}
	seen := map[blockformat.PackRef]bool{}
	var collect func(blockformat.PackRef) error
	collect = func(p blockformat.PackRef) error {
		if seen[p] {
			return nil
		}
		seen[p] = true
		refs[blockformat.Ref{Digest: p.Digest, Size: p.Size}] = packs
		children, segs, e := PackChildren(c, packs, p)
		if e != nil {
			return e
		}
		for _, r := range segs {
			refs[r] = data
		}
		for _, child := range children {
			if e = collect(child); e != nil {
				return e
			}
		}
		return nil
	}
	if e := collect(root.Pack); e != nil {
		return e
	}
	ordered := make([]blockformat.Ref, 0, len(refs))
	for r := range refs {
		ordered = append(ordered, r)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return hex.EncodeToString(ordered[i].Digest[:]) < hex.EncodeToString(ordered[j].Digest[:])
	})
	for _, r := range ordered {
		raw, e := refs[r].get(r)
		if e != nil {
			return e
		}
		if e = l.install(r, raw); e != nil {
			return e
		}
	}
	if e := syncDirectory(filepath.Join(l.dir, "objects")); e != nil {
		return e
	}
	if e := l.boundary("objects-synced"); e != nil {
		return e
	}
	raw, e := json.Marshal(root)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(l.dir, "root-pending-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, e = f.Write(raw); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = l.boundary("root-synced"); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(l.dir, "root")); e != nil {
		return e
	}
	if e = l.boundary("root-installed"); e != nil {
		return e
	}
	if e = syncDirectory(l.dir); e != nil {
		return e
	}
	return l.boundary("root-directory-synced")
}

// Reopen follows only the committed root's physical graph, never a directory scan
// or highest-generation guess. Missing/corrupt committed objects are errors, not
// permission to silently fall back to an older generation.
func (l *Local) Reopen(c *Codec, maxObjects, maxBytes int64) (blockformat.Locator, *Store, *Store, error) {
	fail := func(e error) (blockformat.Locator, *Store, *Store, error) { return blockformat.Locator{}, nil, nil, e }
	if maxObjects <= 0 || maxBytes <= 0 {
		return fail(errors.New("invalid reopen budget"))
	}
	raw, e := readBounded(filepath.Join(l.dir, "root"), 346)
	if e != nil {
		return fail(e)
	}
	var root blockformat.Locator
	if e = json.Unmarshal(raw, &root); e != nil {
		return fail(e)
	}
	data, packs := NewStore(), NewStore()
	seen := map[blockformat.PackRef]bool{}
	segments := map[blockformat.Ref]bool{}
	var count, bytes int64
	read := func(r blockformat.Ref, s *Store) error {
		if r.Size <= 0 || count >= maxObjects || r.Size > maxBytes-bytes {
			return errors.New("reopen budget exceeded")
		}
		count++
		bytes += r.Size
		raw, e := readBounded(l.objectPath(r.Digest), r.Size)
		if e != nil {
			return e
		}
		if e = s.put(r, raw); e != nil {
			return fmt.Errorf("local object: %w", e)
		}
		return nil
	}
	var load func(blockformat.PackRef) error
	load = func(p blockformat.PackRef) error {
		if seen[p] {
			return nil
		}
		if p.Size < 8 || p.Size > 4<<20 || p.Rank < 1 || p.Rank > 6 {
			return errors.New("invalid local pack")
		}
		if e := read(blockformat.Ref{Digest: p.Digest, Size: p.Size}, packs); e != nil {
			return e
		}
		seen[p] = true
		children, segs, e := PackChildren(c, packs, p)
		if e != nil {
			return e
		}
		for _, r := range segs {
			if segments[r] {
				continue
			}
			h, e := blockformat.Header(c.Scope, r)
			if e != nil {
				return e
			}
			if r.Kind != blockformat.SegmentKind || r.Size != int64(len(h))+int64(r.Count)*(blockformat.BlockSize+20) {
				return errors.New("invalid local segment")
			}
			if e = read(r, data); e != nil {
				return e
			}
			segments[r] = true
		}
		for _, child := range children {
			if e = load(child); e != nil {
				return e
			}
		}
		return nil
	}
	if e = load(root.Pack); e != nil {
		return fail(e)
	}
	if _, e = Certify(c, data, packs, root, maxObjects, maxBytes); e != nil {
		return fail(e)
	}
	return root, data, packs, nil
}
