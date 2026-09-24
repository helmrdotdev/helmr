package blockformat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type rangeReply struct {
	raw           []byte
	mutate        func(int, []byte) []byte
	calls, closed int
	requested     int64
	closeErr      error
	cancel        context.CancelFunc
}
type replyBody struct {
	io.Reader
	owner *rangeReply
}

func (b *replyBody) Close() error { b.owner.closed++; return b.owner.closeErr }
func (s *rangeReply) GetRange(ctx context.Context, _ string, size, offset, length int64) (io.ReadCloser, error) {
	s.calls++
	s.requested += length
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size != int64(len(s.raw)) || offset < 0 || offset > size-length {
		return nil, errors.New("source geometry")
	}
	b := bytes.Clone(s.raw[offset : offset+length])
	if s.mutate != nil {
		b = s.mutate(s.calls, b)
	}
	if s.cancel != nil {
		s.cancel()
	}
	return &replyBody{bytes.NewReader(b), s}, nil
}
func recordsFixture(t *testing.T, kind byte, records [][]byte) (Ref, []byte, []byte) {
	t.Helper()
	key := bytes.Repeat([]byte{0x7a}, 32)
	// Fixed salt is confined to each isolated test fixture.
	ref := Ref{Key: "key-version", Kind: kind, Count: uint32(len(records)), Salt: [32]byte{1, 2, 3}}
	ref, raw, err := Seal("scope", key, ref, records)
	if err != nil {
		t.Fatal(err)
	}
	return ref, raw, key
}
func TestBlockRangeReads(t *testing.T) {
	records := make([][]byte, MaxRecords)
	for i := range records {
		records[i] = bytes.Repeat([]byte{byte(i)}, BlockSize)
	}
	ref, raw, key := recordsFixture(t, SegmentKind, records)
	source := &rangeReply{raw: raw}
	got, err := ReadBlock(t.Context(), source, "scope", key, ref, 513)
	if err != nil || !bytes.Equal(got, records[513]) {
		t.Fatalf("block read: %v", err)
	}
	h, _ := Header("scope", ref)
	if source.calls != 2 || source.closed != 2 || source.requested != int64(len(h)+BlockSize+20) {
		t.Fatalf("unbounded reads: %+v", source)
	}
	for _, name := range []string{"short", "long", "header corruption", "record corruption", "wrong ordinal", "close failure", "cancel"} {
		t.Run(name, func(t *testing.T) {
			source := &rangeReply{raw: raw}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "short":
				source.mutate = func(_ int, b []byte) []byte { return b[:len(b)-1] }
			case "long":
				source.mutate = func(_ int, b []byte) []byte { return append(b, 0) }
			case "header corruption":
				source.mutate = func(n int, b []byte) []byte {
					if n == 1 {
						b[0] ^= 1
					}
					return b
				}
			case "record corruption":
				source.mutate = func(n int, b []byte) []byte {
					if n == 2 {
						b[len(b)-1] ^= 1
					}
					return b
				}
			case "wrong ordinal":
				source.mutate = func(n int, b []byte) []byte {
					if n == 2 {
						return bytes.Clone(raw[len(h) : len(h)+BlockSize+20])
					}
					return b
				}
			case "close failure":
				source.closeErr = errors.New("close failed")
			case "cancel":
				source.cancel = cancel
			}
			if got, err := ReadBlock(ctx, source, "scope", key, ref, 513); err == nil || got != nil {
				t.Fatal("failed response returned plaintext")
			}
			if source.closed != source.calls {
				t.Fatal("range body leaked")
			}
		})
	}
	for _, name := range []string{"missing key", "wrong kind", "ordinal", "size", "scope"} {
		t.Run(name, func(t *testing.T) {
			s := &rangeReply{raw: raw}
			r := ref
			k := key
			scope := "scope"
			ordinal := uint32(0)
			switch name {
			case "missing key":
				k = nil
			case "wrong kind":
				r.Kind = RootKind
			case "ordinal":
				ordinal = r.Count
			case "size":
				r.Size++
			case "scope":
				scope = ""
			}
			if _, err := ReadBlock(t.Context(), s, scope, k, r, ordinal); err == nil || s.calls != 0 {
				t.Fatal("invalid request performed IO")
			}
		})
	}
	for _, name := range []string{"wrong key", "wrong scope"} {
		t.Run(name, func(t *testing.T) {
			k := key
			scope := "scope"
			if name == "wrong key" {
				k = bytes.Repeat([]byte{3}, 32)
			} else {
				scope = "other"
			}
			if got, err := ReadBlock(t.Context(), &rangeReply{raw: raw}, scope, k, ref, 0); err == nil || got != nil {
				t.Fatal("unauthorized plaintext")
			}
		})
	}
}
func TestPageRangeReads(t *testing.T) {
	plaintext := []byte(`{"Capacity":32768}`)
	ref, raw, key := recordsFixture(t, RootKind, [][]byte{plaintext})
	pack := append(make([]byte, 128), raw...)
	pack = append(pack, make([]byte, 256)...)
	loc := Locator{Pack: PackRef{Size: int64(len(pack)), Rank: 2}, Page: ref, Offset: 128}
	s := &rangeReply{raw: pack}
	got, err := ReadPage(t.Context(), s, "scope", key, loc)
	if err != nil || !bytes.Equal(got, plaintext) || s.calls != 1 || s.requested != int64(len(raw)) || s.closed != 1 {
		t.Fatalf("page read: %v", err)
	}
	for _, name := range []string{"short", "oversize", "digest", "salt", "offset", "bad bounds", "missing key"} {
		t.Run(name, func(t *testing.T) {
			s := &rangeReply{raw: pack}
			l := loc
			k := key
			switch name {
			case "short":
				s.mutate = func(_ int, b []byte) []byte { return b[:len(b)-1] }
			case "oversize":
				s.mutate = func(_ int, b []byte) []byte { return append(b, 0) }
			case "digest":
				l.Page.Digest[0] ^= 1
			case "salt":
				l.Page.Salt[0] ^= 1
			case "offset":
				l.Offset++
			case "bad bounds":
				l.Page.Size = 1 << 62
			case "missing key":
				k = nil
			}
			if got, err := ReadPage(t.Context(), s, "scope", k, l); err == nil || got != nil {
				t.Fatal("invalid page returned plaintext")
			}
			if s.closed != s.calls {
				t.Fatal("page body leaked")
			}
			if (name == "bad bounds" || name == "missing key") && s.calls != 0 {
				t.Fatal("invalid page request performed IO")
			}
		})
	}
	// Metadata decoding must independently reject short bodies before slicing.
	for n := 0; n < len(raw); n++ {
		if got, err := OpenMetadata("scope", key, ref, raw[:n]); err == nil || got != nil {
			t.Fatal("short metadata accepted")
		}
	}
}
