package blockformat

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
)

// RangeSource opens only the requested range of an immutable object. Adapters
// must check the object's total size, honor cancellation, and never fall back to
// downloading the whole object. The caller owns closing a successful response.
// Optional PackRangeSource and SegmentRangeSource capabilities permit bounded
// read-ahead. Authentication is performed here, independently of the adapter.
type RangeSource interface {
	GetRange(context.Context, string, int64, int64, int64) (io.ReadCloser, error)
}

// PackRangeSource opts into bounded whole-pack caching for metadata pages.
// It returns only the requested ciphertext page bytes; callers authenticate them.
type PackRangeSource interface {
	GetPackRange(context.Context, string, int64, int64, int64) (io.ReadCloser, error)
}

// SegmentRangeSource optionally amortizes adjacent frame reads with bounded
// read-ahead. It returns exactly the requested bytes; authentication stays here.
type SegmentRangeSource interface {
	GetSegmentRange(context.Context, string, int64, int64, int64) (io.ReadCloser, error)
}

type segmentRangeSource struct{ RangeSource }

func (s segmentRangeSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if cached, ok := s.RangeSource.(SegmentRangeSource); ok {
		return cached.GetSegmentRange(ctx, digest, size, offset, length)
	}
	return s.RangeSource.GetRange(ctx, digest, size, offset, length)
}

type metadataRangeSource struct{ RangeSource }

func (s metadataRangeSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if cached, ok := s.RangeSource.(PackRangeSource); ok {
		return cached.GetPackRange(ctx, digest, size, offset, length)
	}
	return s.RangeSource.GetRange(ctx, digest, size, offset, length)
}

func readRange(ctx context.Context, source RangeSource, digest [32]byte, size, offset, length int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || length <= 0 || length > MaxMetadata+1024 || offset < 0 || offset > size-length {
		return nil, errors.New("invalid encrypted range")
	}
	body, err := source.GetRange(ctx, "sha256:"+hex.EncodeToString(digest[:]), size, offset, length)
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("missing encrypted range response")
	}
	b, readErr := io.ReadAll(io.LimitReader(body, length+1))
	err = errors.Join(readErr, body.Close(), ctx.Err())
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != length {
		return nil, errors.New("encrypted range response size mismatch")
	}
	return b, nil
}

// ReadPage reads and authenticates one metadata page, not its complete pack.
// The exact locator must come from a retained root or authenticated parent page.
// Full pack membership/digest certification is a separate publication operation.
func ReadPage(ctx context.Context, source RangeSource, scope string, key []byte, l Locator) ([]byte, error) {
	if source == nil {
		return nil, errors.New("missing metadata source")
	}
	h, err := Header(scope, l.Page)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 || l.Page.Kind == SegmentKind || l.Pack.Size < 8 || l.Pack.Size > 4<<20 || l.Pack.Rank < 1 || l.Pack.Rank > 6 ||
		l.Page.Size < int64(len(h)+20) || l.Page.Size > int64(len(h)+20+MaxMetadata) || l.Offset < 8 || l.Offset > l.Pack.Size-l.Page.Size {
		return nil, errors.New("invalid metadata page range")
	}
	b, err := readRange(ctx, metadataRangeSource{source}, l.Pack.Digest, l.Pack.Size, l.Offset, l.Page.Size)
	if err != nil {
		return nil, err
	}
	return OpenMetadata(scope, key, l.Page, b)
}

// ReadBlock reads only the segment header and one authenticated block frame.
// The caller must have authenticated the parent that supplied this segment ref.
func ReadBlock(ctx context.Context, source RangeSource, scope string, key []byte, r Ref, ordinal uint32) ([]byte, error) {
	return readBlocks(ctx, source, scope, key, r, ordinal, 1)
}

// readBlocks batches only consecutive requested frames. Authentication remains
// per record; a failed later frame never returns an authenticated prefix.
func readBlocks(ctx context.Context, source RangeSource, scope string, key []byte, r Ref, ordinal, count uint32) ([]byte, error) {
	h, err := Header(scope, r)
	if err != nil {
		return nil, err
	}
	const frame = BlockSize + 20
	if len(key) != 32 || r.Kind != SegmentKind || count == 0 || ordinal >= r.Count || count > r.Count-ordinal ||
		count > (MaxMetadata+1024)/frame || r.Size != int64(len(h))+int64(r.Count)*frame {
		return nil, errors.New("invalid segment range")
	}
	header, err := readRange(ctx, source, r.Digest, r.Size, 0, int64(len(h)))
	if err != nil {
		return nil, err
	}
	b, err := readRange(ctx, segmentRangeSource{source}, r.Digest, r.Size, int64(len(h))+int64(ordinal)*frame, int64(count)*frame)
	if err != nil {
		return nil, err
	}
	out := make([]byte, int(count)*BlockSize)
	for i := uint32(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			clear(out)
			return nil, err
		}
		data, err := OpenBlock(scope, key, r, ordinal+i, header, b[int(i)*frame:int(i+1)*frame])
		if err != nil {
			clear(out)
			return nil, err
		}
		copy(out[int(i)*BlockSize:], data)
		clear(data)
	}
	if err := ctx.Err(); err != nil {
		clear(out)
		return nil, err
	}
	return out, nil
}
