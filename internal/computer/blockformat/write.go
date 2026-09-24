package blockformat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
)

// MaxChangedBlocks bounds one capture batch to 4 MiB of changed input,
// independently of disk capacity. Metadata workspace also depends on the touched paths.
// Larger captures can be staged as multiple private generations before publication.
const MaxChangedBlocks = MaxRecords

// MinPackLimit accommodates a full 256-entry node, including distinct segment
// references with maximally escaped 128-byte key IDs, and its encryption/framing.
const MinPackLimit = 1 << 20

// ObjectSink stages immutable bytes outside the guest. Success must retain the
// exact bytes for the candidate's lifetime. It is not a publication authorization.
type ObjectSink interface {
	StoreObject(context.Context, [32]byte, []byte) error
}

// Writer borrows stable scoped keys, an immutable source and a candidate-owned
// sink. PackLimit is the admitted per-object metadata staging budget (1–4 MiB).
// The caller must pin the write key and source before emitting any ciphertext.
type Writer struct {
	Source           RangeSource
	Sink             ObjectSink
	Scope, ActiveKey string
	Keys             map[string][]byte
	PackLimit        int
}

func (w Writer) validate() error {
	if w.Source == nil || w.Sink == nil || len(w.Scope) == 0 || len(w.Scope) > 256 || len(w.ActiveKey) == 0 || len(w.ActiveKey) > 128 || len(w.Keys[w.ActiveKey]) != 32 || w.PackLimit < MinPackLimit || w.PackLimit > 4<<20 {
		return errors.New("invalid generation writer configuration")
	}
	return nil
}
func (w Writer) seal(kind byte, records [][]byte) (Ref, []byte, error) {
	ref := Ref{Key: w.ActiveKey, Kind: kind, Count: uint32(len(records))}
	if _, err := rand.Read(ref.Salt[:]); err != nil {
		return Ref{}, nil, err
	}
	return Seal(w.Scope, w.Keys[ref.Key], ref, records)
}
func (w Writer) store(ctx context.Context, digest [32]byte, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.Sink.StoreObject(ctx, digest, raw); err != nil {
		return err
	}
	return ctx.Err()
}

type blockLocation struct {
	Segment Ref
	Record  uint32
}
type stagedPage struct {
	ref   Ref
	bytes []byte
}
type packDirectory struct {
	Rank  int
	Pages []Ref
}
type packWriter struct {
	writer    Writer
	ctx       context.Context
	converted map[Ref]Locator
}

func encodePack(rank int, pages []stagedPage) ([]byte, int, error) {
	directory := packDirectory{Rank: rank, Pages: make([]Ref, 0, len(pages))}
	for _, page := range pages {
		directory.Pages = append(directory.Pages, page.ref)
	}
	raw, err := json.Marshal(directory)
	if err != nil {
		return nil, 0, err
	}
	var out bytes.Buffer
	out.WriteString("HGP0")
	_ = binary.Write(&out, binary.BigEndian, uint32(len(raw)))
	out.Write(raw)
	offset := out.Len()
	for _, page := range pages {
		out.Write(page.bytes)
	}
	return out.Bytes(), offset, nil
}
func (p *packWriter) publish(rank int, pages []stagedPage) error {
	if len(pages) == 0 {
		return nil
	}
	raw, offset, err := encodePack(rank, pages)
	if err != nil {
		return err
	}
	if len(raw) > p.writer.PackLimit {
		return errors.New("metadata page exceeds pack budget")
	}
	ref := PackRef{Digest: sha256.Sum256(raw), Size: int64(len(raw)), Rank: rank}
	if err = p.writer.store(p.ctx, ref.Digest, raw); err != nil {
		return err
	}
	for _, page := range pages {
		p.converted[page.ref] = Locator{Pack: ref, Page: page.ref, Offset: int64(offset)}
		offset += len(page.bytes)
	}
	return nil
}
