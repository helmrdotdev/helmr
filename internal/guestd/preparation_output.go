package guestd

import (
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

// The mounted producer retains one independently acknowledged sequence per pipe.
// Source saturation drops new bytes while the authored process keeps draining.
type preparationOutput struct{ logs [2]*diagnosticBuffer }

func newPreparationOutput(limits *computerv0.PreparationLogLimits) (*preparationOutput, error) {
	if err := wire.ValidatePreparationLogLimits(limits); err != nil {
		return nil, err
	}
	output := &preparationOutput{}
	for i := range output.logs {
		var err error
		output.logs[i], err = newDiagnosticBuffer(diagnosticLimits{ChunkBytes: int(limits.GetChunkBytes()), BufferBytes: limits.GetBufferBytes(), BufferRecords: int(limits.GetBufferRecords())})
		if err != nil {
			return nil, err
		}
	}
	return output, nil
}
func (o *preparationOutput) stream(stream string) *diagnosticBuffer {
	if stream == "stderr" {
		return o.logs[1]
	}
	return o.logs[0]
}
func (o *preparationOutput) close(complete bool) {
	for _, buffer := range o.logs {
		buffer.close(complete, time.Now())
	}
}
func (o *preparationOutput) pipeClosed(stream string, err error) {
	o.stream(stream).close(err == nil, time.Now())
}
func preparationLog(record diagnosticRecord) *computerv0.PreparationLog {
	kind := map[diagnosticKind]string{diagnosticData: "data", diagnosticGap: "gap", diagnosticEnd: "end"}[record.Kind]
	return &computerv0.PreparationLog{Kind: kind, Sequence: record.Sequence, ThroughSequence: record.Through, ObservedAtUnixNano: record.ObservedAt.UnixNano(), Data: record.Data, DroppedBytes: record.DroppedBytes, Complete: record.Complete}
}

type preparationOutputWriter struct{ buffer *diagnosticBuffer }

func (w preparationOutputWriter) Write(data []byte) (int, error) {
	total := len(data)
	for len(data) > 0 {
		n := min(len(data), w.buffer.limits.ChunkBytes)
		// Bounds and observation are locally constructed. Even accounting exhaustion
		// must drain the process pipe; the buffer records an unknown final boundary.
		_, _ = w.buffer.append(data[:n], time.Now())
		data = data[n:]
	}
	return total, nil
}
