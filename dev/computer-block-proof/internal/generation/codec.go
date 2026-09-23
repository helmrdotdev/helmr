// Package generation is a development-only immutable disk format experiment.
// It has no local durability, publication authority or production integration.
package generation

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const BlockSize = 4096
const maxMetadata = 2 << 20
const maxRecords = 1024
const (
	segmentKind byte = 1
	nodeKind    byte = 2
	rootKind    byte = 3
)

// Ref binds both the random encryption identity and immutable ciphertext identity.
// These experimental JSON representations have no compatibility promise.
type Ref struct {
	Digest [32]byte
	Salt   [32]byte
	Key    string
	Kind   byte
	Count  uint32
	Size   int64
}

type Metrics struct{ Objects, Bytes, Gets, Ranges, ReadBytes int64 }

// Store owns immutable bytes in memory. Reads return copies, never writable views.
// Metrics count actual operations, including repeated and failed lookups.
type Store struct {
	objects map[[32]byte][]byte
	Metrics Metrics
}

func NewStore() *Store { return &Store{objects: make(map[[32]byte][]byte)} }
func (s *Store) put(r Ref, b []byte) error {
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
func (s *Store) get(r Ref) ([]byte, error) {
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
func (s *Store) readRange(r Ref, offset, size int64) ([]byte, error) {
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
func field(b *bytes.Buffer, p []byte) {
	_ = binary.Write(b, binary.BigEndian, uint32(len(p)))
	b.Write(p)
}
func (c *Codec) header(r Ref) ([]byte, error) {
	if len(c.Scope) == 0 || len(c.Scope) > 256 || len(r.Key) == 0 || len(r.Key) > 128 || r.Count == 0 || r.Count > maxRecords || r.Kind < segmentKind || r.Kind > rootKind || (r.Kind != segmentKind && r.Count != 1) {
		return nil, errors.New("invalid object context")
	}
	var b bytes.Buffer
	field(&b, []byte("helmr-generation-proof-v0"))
	field(&b, []byte(c.Scope))
	field(&b, []byte(r.Key))
	b.WriteByte(r.Kind)
	b.Write(r.Salt[:])
	_ = binary.Write(&b, binary.BigEndian, r.Count)
	return b.Bytes(), nil
}
func (c *Codec) aead(r Ref, header []byte) (cipher.AEAD, error) {
	key := c.Keys[r.Key]
	if len(key) != 32 {
		return nil, errors.New("unknown key version")
	}
	derived, err := hkdf.Key(sha256.New, key, r.Salt[:], string(header), 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func recordContext(header []byte, ordinal uint64, length uint32) ([]byte, []byte) {
	nonce := make([]byte, 12)
	copy(nonce, []byte("HGR0"))
	binary.BigEndian.PutUint64(nonce[4:], ordinal)
	var b bytes.Buffer
	b.Write(header)
	_ = binary.Write(&b, binary.BigEndian, ordinal)
	_ = binary.Write(&b, binary.BigEndian, length)
	return nonce, b.Bytes()
}
func (c *Codec) seal(kind byte, records [][]byte) (Ref, []byte, error) {
	r := Ref{Key: c.ActiveKey, Kind: kind, Count: uint32(len(records))}
	if _, err := io.ReadFull(c.entropy, r.Salt[:]); err != nil {
		return Ref{}, nil, err
	}
	h, err := c.header(r)
	if err != nil {
		return Ref{}, nil, err
	}
	a, err := c.aead(r, h)
	if err != nil {
		return Ref{}, nil, err
	}
	var out bytes.Buffer
	out.Write(h)
	for i, p := range records {
		if (kind == segmentKind && len(p) != BlockSize) || len(p) > maxMetadata {
			return Ref{}, nil, errors.New("invalid record length")
		}
		ordinal := uint64(i)
		if kind == segmentKind {
			ordinal++
		}
		nonce, aad := recordContext(h, ordinal, uint32(len(p)))
		_ = binary.Write(&out, binary.BigEndian, uint32(len(p)))
		out.Write(a.Seal(nil, nonce, p, aad))
	}
	b := out.Bytes()
	r.Size = int64(len(b))
	r.Digest = sha256.Sum256(b)
	return r, b, nil
}
func (c *Codec) metadata(s *Store, r Ref) ([]byte, error) {
	if r.Kind == segmentKind {
		return nil, errors.New("metadata required")
	}
	h, err := c.header(r)
	if err != nil {
		return nil, err
	}
	if r.Size < int64(len(h)+20) || r.Size > int64(len(h)+20+maxMetadata) {
		return nil, errors.New("metadata bounds")
	}
	b, err := s.get(r)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(b) != r.Digest || !bytes.Equal(b[:len(h)], h) {
		return nil, errors.New("metadata identity mismatch")
	}
	n := binary.BigEndian.Uint32(b[len(h):])
	if int(n) != len(b)-len(h)-20 {
		return nil, errors.New("metadata record size")
	}
	a, err := c.aead(r, h)
	if err != nil {
		return nil, err
	}
	nonce, aad := recordContext(h, 0, n)
	return a.Open(nil, nonce, b[len(h)+4:], aad)
}
func (c *Codec) block(s *Store, r Ref, ordinal uint32) ([]byte, error) {
	if r.Kind != segmentKind || ordinal >= r.Count {
		return nil, errors.New("invalid block reference")
	}
	h, err := c.header(r)
	if err != nil {
		return nil, err
	}
	const frame = BlockSize + 4 + 16
	if r.Size != int64(len(h))+int64(r.Count)*frame {
		return nil, errors.New("segment geometry mismatch")
	}
	stored, err := s.readRange(r, 0, int64(len(h)))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(stored, h) {
		return nil, errors.New("segment header mismatch")
	}
	b, err := s.readRange(r, int64(len(h))+int64(ordinal)*frame, frame)
	if err != nil {
		return nil, err
	}
	if len(b) != frame || binary.BigEndian.Uint32(b) != BlockSize {
		return nil, errors.New("range length mismatch")
	}
	a, err := c.aead(r, h)
	if err != nil {
		return nil, err
	}
	nonce, aad := recordContext(h, uint64(ordinal)+1, BlockSize)
	p, err := a.Open(nil, nonce, b[4:], aad)
	if err != nil {
		return nil, fmt.Errorf("block authentication: %w", err)
	}
	return p, nil
}
