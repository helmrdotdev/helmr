// Package blockproof is a development-only, local-file object/block model.
package blockproof

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const BlockSize = 4096

var ErrCapacity = errors.New("dirty block capacity exhausted")
var ErrConflict = errors.New("publication fence conflict")

// Store models immutable remote objects with local files. Fault injection runs
// before an object write. The caller must not mutate objects except in tests.
type Store struct {
	dir       string
	mu        sync.Mutex
	Reads     int
	BeforePut func() error
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}
func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func (s *Store) put(b []byte) (string, error) {
	if s.BeforePut != nil {
		if err := s.BeforePut(); err != nil {
			return "", err
		}
	}
	id := digest(b)
	f, err := os.CreateTemp(s.dir, "upload-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return "", err
	}
	// Linking rather than overwriting keeps an existing content-addressed object immutable.
	if err = os.Link(name, filepath.Join(s.dir, id)); err != nil && !os.IsExist(err) {
		return "", err
	}
	existing, err := os.ReadFile(filepath.Join(s.dir, id))
	if err != nil {
		return "", err
	}
	if !bytes.Equal(existing, b) {
		return "", errors.New("existing object corrupt")
	}
	return id, nil
}
func (s *Store) read(id string, off int64, n int) ([]byte, error) {
	if len(id) != 64 {
		return nil, errors.New("invalid object id")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.Reads++
	s.mu.Unlock()
	f, err := os.Open(filepath.Join(s.dir, id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	_, err = f.ReadAt(b, off)
	return b, err
}
func (s *Store) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.Reads }

type mapping struct {
	Segment string
	Offset  int64
	Hash    string
}
type manifest struct {
	Size   int64
	Blocks map[int64]mapping
}

func (s *Store) load(id string) (manifest, error) {
	var m manifest
	b, err := s.read(id, 0, 1)
	_ = b // Validate the identifier before constructing a path.
	if err != nil {
		return m, err
	}
	b, err = os.ReadFile(filepath.Join(s.dir, id))
	if err != nil {
		return m, err
	}
	if digest(b) != id {
		return m, errors.New("manifest checksum mismatch")
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if m.Size <= 0 || m.Size%BlockSize != 0 || m.Blocks == nil {
		return m, errors.New("invalid geometry")
	}
	for k, v := range m.Blocks {
		if k < 0 || k >= m.Size/BlockSize || v.Offset < 0 || v.Offset%BlockSize != 0 || v.Offset > int64(^uint64(0)>>1)-BlockSize {
			return m, errors.New("invalid mapping")
		}
	}
	return m, nil
}
func (s *Store) block(r mapping) ([]byte, error) {
	b, err := s.read(r.Segment, r.Offset, BlockSize)
	if err != nil {
		return nil, err
	}
	if digest(b) != r.Hash {
		return nil, errors.New("block checksum mismatch")
	}
	return b, nil
}

// Disk serializes foreground operations. Capture detaches a frozen dirty map;
// writes can proceed while uploads run, bounded by dirty + frozen block count.
type Disk struct {
	mu     sync.Mutex
	store  *Store
	size   int64
	limit  int
	base   map[int64]mapping
	dirty  map[int64][]byte
	frozen map[int64][]byte
}

func NewDisk(s *Store, size int64, limit int) (*Disk, error) {
	if size <= 0 || size%BlockSize != 0 || limit <= 0 {
		return nil, errors.New("invalid geometry or capacity")
	}
	return &Disk{store: s, size: size, limit: limit, base: map[int64]mapping{}, dirty: map[int64][]byte{}}, nil
}
func Restore(s *Store, id string, limit int) (*Disk, error) {
	m, err := s.load(id)
	if err != nil {
		return nil, err
	}
	d, err := NewDisk(s, m.Size, limit)
	if err == nil {
		d.base = m.Blocks
	}
	return d, err
}
func (d *Disk) bounds(off int64, n int) bool {
	return off >= 0 && off <= d.size && int64(n) <= d.size-off
}
func (d *Disk) block(k int64) ([]byte, error) {
	if b, ok := d.dirty[k]; ok {
		return b, nil
	}
	if b, ok := d.frozen[k]; ok {
		return b, nil
	}
	if r, ok := d.base[k]; ok {
		return d.store.block(r)
	}
	return make([]byte, BlockSize), nil
}
func (d *Disk) ReadAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.bounds(off, len(p)) {
		return 0, io.EOF
	}
	for n := 0; n < len(p); {
		k := (off + int64(n)) / BlockSize
		start := int((off + int64(n)) % BlockSize)
		b, err := d.block(k)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], b[start:])
	}
	return len(p), nil
}
func (d *Disk) WriteAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.bounds(off, len(p)) {
		return 0, io.ErrShortWrite
	}
	if len(p) == 0 {
		return 0, nil
	}
	first, last := off/BlockSize, (off+int64(len(p))-1)/BlockSize
	needed := len(d.dirty) + len(d.frozen)
	for k := first; k <= last; k++ {
		if _, ok := d.dirty[k]; !ok {
			needed++
		}
	}
	if needed > d.limit {
		return 0, ErrCapacity
	}
	// Resolve all read-modify-write dependencies before changing any block.
	pending := map[int64][]byte{}
	for k := first; k <= last; k++ {
		lo := max(off, k*BlockSize)
		hi := min(off+int64(len(p)), (k+1)*BlockSize)
		b := make([]byte, BlockSize)
		if hi-lo < BlockSize {
			old, err := d.block(k)
			if err != nil {
				return 0, err
			}
			copy(b, old)
		}
		copy(b[lo-k*BlockSize:hi-k*BlockSize], p[lo-off:hi-off])
		pending[k] = b
	}
	for k, b := range pending {
		d.dirty[k] = b
	}
	return len(p), nil
}

// Trim models aligned discard as explicit zero; omission in the complete index
// means zero. Partial discard is unsupported in this bounded proof.
func (d *Disk) Trim(off int64, n int) error {
	if off%BlockSize != 0 || n%BlockSize != 0 {
		return errors.New("unaligned trim")
	}
	if n < 0 || !d.bounds(off, n) {
		return io.ErrShortWrite
	}
	if n/BlockSize > d.limit {
		return ErrCapacity
	}
	_, err := d.WriteAt(make([]byte, n), off)
	return err
}

// Capture stages immutable objects and a complete logical index. Returned IDs
// are pins, not mutable latest pointers. Only one capture per Disk may run.
func (d *Disk) Capture() (string, error) {
	d.mu.Lock()
	if d.frozen != nil {
		d.mu.Unlock()
		return "", ErrCapacity
	}
	frozen := d.dirty
	d.frozen = frozen
	d.dirty = map[int64][]byte{}
	index := make(map[int64]mapping, len(d.base))
	for k, v := range d.base {
		index[k] = v
	}
	d.mu.Unlock()
	success := false
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if success {
			d.base = index
		} else {
			for k, b := range frozen {
				if _, ok := d.dirty[k]; !ok {
					d.dirty[k] = b
				}
			}
		}
		d.frozen = nil
	}()
	keys := make([]int64, 0, len(frozen))
	for k := range frozen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var segment []byte
	offsets := map[int64]int64{}
	for _, k := range keys {
		b := frozen[k]
		if bytes.Equal(b, make([]byte, BlockSize)) {
			delete(index, k)
			continue
		}
		offsets[k] = int64(len(segment))
		segment = append(segment, b...)
	}
	if len(segment) > 0 {
		id, err := d.store.put(segment)
		if err != nil {
			return "", err
		}
		for k, off := range offsets {
			index[k] = mapping{id, off, digest(frozen[k])}
		}
	}
	b, err := json.Marshal(manifest{d.size, index})
	if err != nil {
		return "", err
	}
	id, err := d.store.put(b)
	if err != nil {
		return "", err
	}
	success = true
	return id, nil
}

// Head is a process-local stand-in for an authoritative transactional register.
// Publication validates the entire referenced set before the fenced CAS.
// It does not implement a distributed lease or persistent database transaction.
type Head struct {
	mu    sync.Mutex
	epoch uint64
	id    string
}

func (h *Head) Claim() uint64 { h.mu.Lock(); defer h.mu.Unlock(); h.epoch++; return h.epoch }
func (h *Head) ID() string    { h.mu.Lock(); defer h.mu.Unlock(); return h.id }
func (h *Head) Publish(s *Store, epoch uint64, expected, id string) error {
	m, err := s.load(id)
	if err != nil {
		return err
	}
	for k, r := range m.Blocks {
		if _, err = s.block(r); err != nil {
			return fmt.Errorf("block %d: %w", k, err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if epoch == 0 || epoch != h.epoch || expected != h.id {
		return ErrConflict
	}
	h.id = id
	return nil
}
