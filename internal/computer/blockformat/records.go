package blockformat

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const BlockSize = 4096
const MaxMetadata = 2 << 20
const MaxRecords = 1024
const (
	SegmentKind byte = 1
	NodeKind    byte = 2
	RootKind    byte = 3
)

func field(b *bytes.Buffer, p []byte) {
	_ = binary.Write(b, binary.BigEndian, uint32(len(p)))
	b.Write(p)
}
func Header(scope string, r Ref) ([]byte, error) {
	if len(scope) == 0 || len(scope) > 256 || len(r.Key) == 0 || len(r.Key) > 128 || r.Count == 0 || r.Count > MaxRecords || r.Kind < SegmentKind || r.Kind > RootKind || (r.Kind != SegmentKind && r.Count != 1) {
		return nil, errors.New("invalid object context")
	}
	var b bytes.Buffer
	field(&b, []byte("helmr-computer-generation-v1"))
	field(&b, []byte(scope))
	field(&b, []byte(r.Key))
	b.WriteByte(r.Kind)
	b.Write(r.Salt[:])
	_ = binary.Write(&b, binary.BigEndian, r.Count)
	return b.Bytes(), nil
}
func recordAEAD(key []byte, r Ref, header []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("unknown key version")
	}
	derived, err := hkdf.Key(sha256.New, key, r.Salt[:], string(header), 32)
	if err != nil {
		return nil, err
	}
	defer clear(derived)
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func recordContext(header []byte, ordinal uint64, length uint32) ([]byte, []byte) {
	nonce := make([]byte, 12)
	copy(nonce, []byte("HGR1"))
	binary.BigEndian.PutUint64(nonce[4:], ordinal)
	var b bytes.Buffer
	b.Write(header)
	_ = binary.Write(&b, binary.BigEndian, ordinal)
	_ = binary.Write(&b, binary.BigEndian, length)
	return nonce, b.Bytes()
}

// Seal encrypts records using a fresh random salt supplied by the trusted writer.
// A salt must never be reused with the same scope/key for different plaintext.
func Seal(scope string, key []byte, r Ref, records [][]byte) (Ref, []byte, error) {
	if len(records) != int(r.Count) || r.Size != 0 || r.Digest != ([32]byte{}) {
		return Ref{}, nil, errors.New("invalid new record descriptor")
	}
	h, err := Header(scope, r)
	if err != nil {
		return Ref{}, nil, err
	}
	a, err := recordAEAD(key, r, h)
	if err != nil {
		return Ref{}, nil, err
	}
	var out bytes.Buffer
	out.Write(h)
	for i, p := range records {
		if (r.Kind == SegmentKind && len(p) != BlockSize) || len(p) > MaxMetadata {
			return Ref{}, nil, errors.New("invalid record length")
		}
		ordinal := uint64(i)
		if r.Kind == SegmentKind {
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

// OpenMetadata verifies the complete encrypted metadata identity before decryption.
func OpenMetadata(scope string, key []byte, r Ref, b []byte) ([]byte, error) {
	if r.Kind == SegmentKind {
		return nil, errors.New("metadata required")
	}
	h, err := Header(scope, r)
	if err != nil {
		return nil, err
	}
	if r.Size < int64(len(h)+20) || r.Size > int64(len(h)+20+MaxMetadata) {
		return nil, errors.New("metadata bounds")
	}
	if int64(len(b)) != r.Size {
		return nil, errors.New("metadata response size mismatch")
	}
	if sha256.Sum256(b) != r.Digest || !bytes.Equal(b[:len(h)], h) {
		return nil, errors.New("metadata identity mismatch")
	}
	n := binary.BigEndian.Uint32(b[len(h):])
	if int(n) != len(b)-len(h)-20 {
		return nil, errors.New("metadata record size")
	}
	a, err := recordAEAD(key, r, h)
	if err != nil {
		return nil, err
	}
	nonce, aad := recordContext(h, 0, n)
	return a.Open(nil, nonce, b[len(h)+4:], aad)
}

// OpenBlock authenticates one range against a trusted, retained segment descriptor.
// It does not certify the digest of unrequested segment records.
func OpenBlock(scope string, key []byte, r Ref, ordinal uint32, stored, b []byte) ([]byte, error) {
	if r.Kind != SegmentKind || ordinal >= r.Count {
		return nil, errors.New("invalid block reference")
	}
	h, err := Header(scope, r)
	if err != nil {
		return nil, err
	}
	const frame = BlockSize + 4 + 16
	if r.Size != int64(len(h))+int64(r.Count)*frame {
		return nil, errors.New("segment geometry mismatch")
	}
	if !bytes.Equal(stored, h) {
		return nil, errors.New("segment header mismatch")
	}
	if len(b) != frame || binary.BigEndian.Uint32(b) != BlockSize {
		return nil, errors.New("range length mismatch")
	}
	a, err := recordAEAD(key, r, h)
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
