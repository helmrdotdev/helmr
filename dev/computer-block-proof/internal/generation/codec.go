// Package generation is a development-only immutable disk format experiment.
// Local persistence experiments do not provide publication authority or production integration.
package generation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

type Metrics struct{ Objects, Bytes, Gets, Ranges, ReadBytes int64 }

// Store owns immutable bytes in memory. Reads return copies, never writable views.
// Metrics count actual operations, including repeated and failed lookups.
type Store struct {
	objects map[[32]byte][]byte
	Metrics Metrics
}

func NewStore() *Store { return &Store{objects: make(map[[32]byte][]byte)} }
func (s *Store) put(r blockformat.Ref, b []byte) error {
	if int64(len(b)) != r.Size || sha256.Sum256(b) != r.Digest {
		return errors.New("object identity mismatch")
	}
	if old, ok := s.objects[r.Digest]; ok {
		if !bytes.Equal(old, b) {
			return errors.New("immutable collision")
		}
		return nil
	}
	s.objects[r.Digest] = bytes.Clone(b)
	s.Metrics.Objects++
	s.Metrics.Bytes += int64(len(b))
	return nil
}
func (s *Store) get(r blockformat.Ref) ([]byte, error) {
	s.Metrics.Gets++
	b, ok := s.objects[r.Digest]
	if !ok {
		return nil, errors.New("missing object")
	}
	if int64(len(b)) != r.Size {
		return nil, errors.New("object size mismatch")
	}
	s.Metrics.ReadBytes += int64(len(b))
	return bytes.Clone(b), nil
}
func (s *Store) readRange(r blockformat.Ref, offset, size int64) ([]byte, error) {
	s.Metrics.Ranges++
	b, ok := s.objects[r.Digest]
	if !ok {
		return nil, errors.New("missing object")
	}
	if int64(len(b)) != r.Size || offset < 0 || size < 0 || offset > int64(len(b))-size {
		return nil, errors.New("invalid range")
	}
	s.Metrics.ReadBytes += size
	return bytes.Clone(b[offset : offset+size]), nil
}

// Codec receives already-scoped versioned keys. It does not provision keys or
// infer authorization from a scope label. Old versions remain readable if retained.
type Codec struct {
	Scope     string
	ActiveKey string
	Keys      map[string][]byte
	entropy   io.Reader
}

func NewCodec(scope, active string, keys map[string][]byte) (*Codec, error) {
	c := &Codec{Scope: scope, ActiveKey: active, Keys: make(map[string][]byte), entropy: rand.Reader}
	if len(scope) == 0 || len(scope) > 256 {
		return nil, errors.New("invalid scope")
	}
	for id, key := range keys {
		if len(id) == 0 || len(id) > 128 || len(key) != 32 {
			return nil, errors.New("invalid key")
		}
		c.Keys[id] = bytes.Clone(key)
	}
	if c.Keys[active] == nil {
		return nil, errors.New("missing active key")
	}
	return c, nil
}
func (c *Codec) seal(kind byte, records [][]byte) (blockformat.Ref, []byte, error) {
	r := blockformat.Ref{Key: c.ActiveKey, Kind: kind, Count: uint32(len(records))}
	if _, err := io.ReadFull(c.entropy, r.Salt[:]); err != nil {
		return blockformat.Ref{}, nil, err
	}
	return blockformat.Seal(c.Scope, c.Keys[r.Key], r, records)
}
func (c *Codec) metadata(s *Store, r blockformat.Ref) ([]byte, error) {
	h, err := blockformat.Header(c.Scope, r)
	if err != nil {
		return nil, err
	}
	if r.Kind == blockformat.SegmentKind || r.Size < int64(len(h)+20) || r.Size > int64(len(h)+20+blockformat.MaxMetadata) {
		return nil, errors.New("invalid metadata bounds")
	}
	b, err := s.get(r)
	if err != nil {
		return nil, err
	}
	return blockformat.OpenMetadata(c.Scope, c.Keys[r.Key], r, b)
}
func (c *Codec) block(s *Store, r blockformat.Ref, ordinal uint32) ([]byte, error) {
	return blockformat.ReadBlock(context.Background(), storeRanges{s}, c.Scope, c.Keys[r.Key], r, ordinal)
}

type storeRanges struct{ store *Store }

func (s storeRanges) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("invalid range digest")
	}
	var hash [32]byte
	copy(hash[:], raw)
	b, err := s.store.readRange(blockformat.Ref{Digest: hash, Size: size}, offset, length)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// The proof stores data and packs separately. Select the known object namespace
// before reading; a failed read is never retried against the other store.
type treeRanges struct{ data, packs *Store }

func (s treeRanges) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("invalid range digest")
	}
	var hash [32]byte
	copy(hash[:], raw)
	if _, ok := s.packs.objects[hash]; ok {
		return (storeRanges{s.packs}).GetRange(ctx, digest, size, offset, length)
	}
	return (storeRanges{s.data}).GetRange(ctx, digest, size, offset, length)
}
