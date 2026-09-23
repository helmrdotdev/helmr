package generation

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
)

// MarshalJSON uses a fixed binary descriptor inside a JSON byte string. This
// unshipped experiment has one encoding, not a legacy-format fallback. Both
// physical and page identities remain authenticated; no digest is removed.
func (l Locator) MarshalJSON() ([]byte, error) {
	if len(l.Page.Key) > 128 || l.Pack.Rank < 0 || l.Pack.Rank > 255 || l.Pack.Size < 0 || l.Page.Size < 0 || l.Offset < 0 {
		return nil, errors.New("locator encoding bounds")
	}
	b := make([]byte, 128+len(l.Page.Key))
	copy(b[:32], l.Pack.Digest[:])
	binary.BigEndian.PutUint64(b[32:40], uint64(l.Pack.Size))
	b[40] = byte(l.Pack.Rank)
	copy(b[41:73], l.Page.Digest[:])
	copy(b[73:105], l.Page.Salt[:])
	b[105] = l.Page.Kind
	binary.BigEndian.PutUint32(b[106:110], l.Page.Count)
	binary.BigEndian.PutUint64(b[110:118], uint64(l.Page.Size))
	binary.BigEndian.PutUint64(b[118:126], uint64(l.Offset))
	binary.BigEndian.PutUint16(b[126:128], uint16(len(l.Page.Key)))
	copy(b[128:], l.Page.Key)
	return json.Marshal(b)
}
func (l *Locator) UnmarshalJSON(raw []byte) error {
	// 256 binary bytes need at most 344 base64 characters plus quotes. Reject
	// oversized escaped encodings before the JSON decoder allocates their payload.
	if len(raw) < 2 || len(raw) > 346 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return errors.New("locator encoding size")
	}
	var b []byte
	if err := json.Unmarshal(raw, &b); err != nil {
		return err
	}
	if len(b) < 128 || len(b) > 256 || int(binary.BigEndian.Uint16(b[126:128])) != len(b)-128 {
		return errors.New("locator encoding length")
	}
	size := binary.BigEndian.Uint64(b[32:40])
	pageSize := binary.BigEndian.Uint64(b[110:118])
	offset := binary.BigEndian.Uint64(b[118:126])
	if size > math.MaxInt64 || pageSize > math.MaxInt64 || offset > math.MaxInt64 {
		return errors.New("locator integer overflow")
	}
	var out Locator
	copy(out.Pack.Digest[:], b[:32])
	out.Pack.Size = int64(size)
	out.Pack.Rank = int(b[40])
	copy(out.Page.Digest[:], b[41:73])
	copy(out.Page.Salt[:], b[73:105])
	out.Page.Kind = b[105]
	out.Page.Count = binary.BigEndian.Uint32(b[106:110])
	out.Page.Size = int64(pageSize)
	out.Offset = int64(offset)
	out.Page.Key = string(b[128:])
	*l = out
	return nil
}
