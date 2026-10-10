package diagnostic

import (
	"math"
	"time"
)

// Bounds cap pending payloads. The two fixed pipe records live on their owner.
type Bounds struct {
	ChunkBytes, SourceBytes, SourceRecords, EnvironmentBytes, EnvironmentRecords, QueueBytes, QueueRecords int64
}

func (b Bounds) Validate() error {
	if b.ChunkBytes <= 0 || b.SourceBytes < b.ChunkBytes || b.EnvironmentBytes < b.SourceBytes || b.QueueBytes < b.EnvironmentBytes || b.SourceRecords <= 0 || b.EnvironmentRecords < b.SourceRecords || b.QueueRecords < b.EnvironmentRecords || b.QueueRecords == math.MaxInt64 {
		return ErrInvalid
	}
	return nil
}

type ExportBounds struct {
	Records  int
	Bytes    int64
	ClaimFor time.Duration
}

func (b ExportBounds) Validate() error {
	if b.Records <= 0 || b.Bytes <= 0 || b.ClaimFor < time.Microsecond {
		return ErrInvalid
	}
	return nil
}

type IngestConfig struct {
	Admission        Bounds
	Batch            ExportBounds
	PollEvery        time.Duration
	RetryAfter       time.Duration
	OperationTimeout time.Duration
	GCBatchRecords   int
}

func (c IngestConfig) Validate() error {
	if c.Admission.Validate() != nil || c.Batch.Validate() != nil || c.Batch.Bytes < c.Admission.ChunkBytes || c.PollEvery <= 0 || c.RetryAfter < time.Microsecond || c.OperationTimeout <= 0 || c.OperationTimeout >= c.Batch.ClaimFor || c.GCBatchRecords <= 0 {
		return ErrInvalid
	}
	return nil
}
