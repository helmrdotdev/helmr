package diagnostic

import (
	"errors"
	"math"
)

var ErrInvalid = errors.New("invalid diagnostic record or bounds")

type Record struct {
	Stream             string
	Kind               string
	Sequence           int64
	ThroughSequence    int64
	ObservedAtUnixNano int64
	Data               []byte
	DroppedBytes       int64
	Complete           bool
}

func (r Record) Validate(chunkBytes int64) error {
	if chunkBytes <= 0 || (r.Stream != "stdout" && r.Stream != "stderr") || r.Sequence <= 0 || r.ThroughSequence < r.Sequence || r.ObservedAtUnixNano <= 0 || int64(len(r.Data)) > chunkBytes {
		return ErrInvalid
	}
	switch r.Kind {
	case "data":
		if r.Sequence != r.ThroughSequence || r.ThroughSequence == math.MaxInt64 || len(r.Data) == 0 || r.DroppedBytes != 0 || r.Complete {
			return ErrInvalid
		}
	case "gap":
		if r.ThroughSequence == math.MaxInt64 || len(r.Data) != 0 || r.DroppedBytes < r.ThroughSequence-r.Sequence+1 || r.Complete {
			return ErrInvalid
		}
	case "end":
		if r.Sequence != r.ThroughSequence || len(r.Data) != 0 || r.DroppedBytes != 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
