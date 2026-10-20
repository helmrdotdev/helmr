// Package bootgate owns the pre-mount bootstrap boundary. Its process exits
// before guest control or customer processes start; no connection or authority
// from a seed builder is carried into a running Computer.
package bootgate

import (
	"encoding/binary"
	"errors"
	"io"
	"time"
)

const (
	Port           = 5002
	Protocol       = "helmr.boot-gate.v1\n"
	Probe          = byte(0)
	Resume         = byte(1)
	EntropyBytes   = 64
	ChallengeBytes = 16
	RequestBytes   = 8 + EntropyBytes + ChallengeBytes
	ReceiptBytes   = ChallengeBytes + 36 + 32
)

type Request struct {
	Time      time.Time
	Entropy   [EntropyBytes]byte
	Challenge [ChallengeBytes]byte
}

func (r Request) Encode() []byte {
	raw := make([]byte, RequestBytes)
	binary.BigEndian.PutUint64(raw, uint64(r.Time.UnixNano()))
	copy(raw[8:], r.Entropy[:])
	copy(raw[8+EntropyBytes:], r.Challenge[:])
	return raw
}

func ReadRequest(reader io.Reader) (Request, error) {
	var raw [RequestBytes]byte
	defer clear(raw[:])
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return Request{}, err
	}
	nanos := int64(binary.BigEndian.Uint64(raw[:8]))
	if nanos <= 0 {
		return Request{}, errors.New("bootstrap time is invalid")
	}
	r := Request{Time: time.Unix(0, nanos)}
	copy(r.Entropy[:], raw[8:8+EntropyBytes])
	copy(r.Challenge[:], raw[8+EntropyBytes:])
	if r.Entropy == [EntropyBytes]byte{} || r.Challenge == [ChallengeBytes]byte{} {
		return Request{}, errors.New("bootstrap freshness is missing")
	}
	return r, nil
}
