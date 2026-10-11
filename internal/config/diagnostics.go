package config

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
)

// DiagnosticAdmission gives log ingestion its own connection budget and bounded
// retained queue. Lifecycle requests use the ordinary database pool.
type DiagnosticAdmission struct {
	Bounds         diagnostic.Bounds
	MaxConnections int32
}

func LoadDiagnosticAdmission() (DiagnosticAdmission, error) {
	var c DiagnosticAdmission
	for _, field := range []struct {
		name   string
		target *int64
	}{
		{"DIAGNOSTIC_CHUNK_BYTES", &c.Bounds.ChunkBytes},
		{"DIAGNOSTIC_SOURCE_BYTES", &c.Bounds.SourceBytes},
		{"DIAGNOSTIC_SOURCE_RECORDS", &c.Bounds.SourceRecords},
		{"DIAGNOSTIC_ENVIRONMENT_BYTES", &c.Bounds.EnvironmentBytes},
		{"DIAGNOSTIC_ENVIRONMENT_RECORDS", &c.Bounds.EnvironmentRecords},
		{"DIAGNOSTIC_QUEUE_BYTES", &c.Bounds.QueueBytes},
		{"DIAGNOSTIC_QUEUE_RECORDS", &c.Bounds.QueueRecords},
	} {
		n, err := strconv.ParseInt(envText(field.name), 10, 64)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("%s must be explicitly configured as a positive integer", field.name)
		}
		*field.target = n
	}
	n, err := strconv.ParseInt(envText("DIAGNOSTIC_DB_MAX_CONNECTIONS"), 10, 32)
	if err != nil || n <= 0 || n > math.MaxInt32 {
		return c, fmt.Errorf("DIAGNOSTIC_DB_MAX_CONNECTIONS must be explicitly configured as a positive int32")
	}
	c.MaxConnections = int32(n)
	if c.Bounds.Validate() != nil || c.Bounds.ChunkBytes > 16<<20 {
		return c, fmt.Errorf("diagnostic queue bounds must be ordered and chunks must fit the 16 MiB transport limit")
	}
	return c, nil
}

// DiagnosticExporter bounds sink batches separately from lifecycle work.
// Claim lifetime exceeds the bounded operation deadline, so a failed sender
// cannot race a replacement sender before its operation has timed out.
type DiagnosticExporter struct {
	MaxConnections int32
	Ingest         diagnostic.IngestConfig
}

func LoadDiagnosticExporter() (DiagnosticExporter, error) {
	admission, err := LoadDiagnosticAdmission()
	if err != nil {
		return DiagnosticExporter{}, err
	}
	records, err := strconv.ParseInt(envText("DIAGNOSTIC_EXPORT_BATCH_RECORDS"), 10, 32)
	if err != nil || records <= 0 {
		return DiagnosticExporter{}, fmt.Errorf("DIAGNOSTIC_EXPORT_BATCH_RECORDS must be explicitly configured as a positive int32")
	}
	bytes, err := strconv.ParseInt(envText("DIAGNOSTIC_EXPORT_BATCH_BYTES"), 10, 64)
	if err != nil || bytes < admission.Bounds.ChunkBytes {
		return DiagnosticExporter{}, fmt.Errorf("DIAGNOSTIC_EXPORT_BATCH_BYTES must be explicitly configured to fit at least one diagnostic chunk")
	}
	c := DiagnosticExporter{MaxConnections: admission.MaxConnections, Ingest: diagnostic.IngestConfig{
		Admission: admission.Bounds, Batch: diagnostic.ExportBounds{Records: int(records), Bytes: bytes, ClaimFor: 30 * time.Second},
		PollEvery: time.Second, RetryAfter: 2 * time.Second, OperationTimeout: 25 * time.Second, GCBatchRecords: 2500,
	}}
	return c, c.Ingest.Validate()
}
