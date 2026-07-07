package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
)

var ErrHistoricalUnavailable = errors.New("telemetry historical store unavailable")

type LaggingError struct {
	WatermarkSeq int64
	WantSeq      int64
}

func (e LaggingError) Error() string {
	return "telemetry replay is lagging"
}

type Reader interface {
	ListEvents(ctx context.Context, query EventQuery) (EventPage, error)
	ListRunLogChunks(ctx context.Context, query RunLogChunkQuery) (RunLogChunkPage, error)
	ListTerminalOutput(ctx context.Context, query TerminalOutputQuery) (TerminalOutputPage, error)
	GetRunLogSnapshot(ctx context.Context, query RunLogSnapshotQuery) (RunLogSnapshot, error)
}

type EventQuery struct {
	OrgID         uuid.UUID
	WorkerGroupID string
	SubjectType   string
	SubjectID     uuid.UUID
	AfterSeq      int64
	Limit         int32
}

type EventPage struct {
	Events     []api.RunEvent
	LastSeq    int64
	Watermark  int64
	HotCount   int
	Historical int
}

type RunLogChunkQuery struct {
	OrgID         uuid.UUID
	WorkerGroupID string
	RunID         uuid.UUID
	AfterSeq      int64
	Limit         int32
}

type RunLogChunkPage struct {
	Chunks     []api.RunLogChunk
	LastSeq    int64
	Watermark  int64
	HotCount   int
	Historical int
}

type RunLogSnapshotQuery struct {
	OrgID         uuid.UUID
	WorkerGroupID string
	RunID         uuid.UUID
	StdoutLimit   int64
	StderrLimit   int64
}

type RunLogSnapshot struct {
	Stdout      []byte
	Stderr      []byte
	Cursor      int64
	StdoutBytes int64
	StderrBytes int64
	Truncated   bool
	UpdatedAt   time.Time
}

type DeadLetteredTelemetryQuery struct {
	OrgID         uuid.UUID
	WorkerGroupID string
	StreamKind    string
	SourceKind    string
	SourceID      uuid.UUID
	AfterSeq      int64
	Watermark     int64
}

type TerminalOutputQuery struct {
	OrgID         uuid.UUID
	WorkerGroupID string
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	WorkspaceID   uuid.UUID
	ResourceKind  string
	ResourceID    uuid.UUID
	StreamName    string
	AfterOffset   int64
	Limit         int32
}

type TerminalOutputPage struct {
	Chunks     []TerminalOutputChunk
	LastOffset int64
	Watermark  int64
	HotCount   int
	Historical int
}

type TerminalOutputChunk struct {
	ID          string
	Stream      string
	OffsetStart int64
	OffsetEnd   int64
	Data        []byte
	ObservedAt  time.Time
	CreatedAt   time.Time
}

type IngestWriter interface {
	WriteEvents(context.Context, []EventRecord) error
	WriteRunLogs(context.Context, []RunLogRecord) error
	WriteMeterEvents(context.Context, []MeterEventRecord) error
	WriteTerminalOutput(context.Context, []TerminalOutputRecord) error
}

type MeterEventRecord struct {
	WorkerGroupID  string     `json:"worker_group_id"`
	OrgID          uuid.UUID  `json:"org_id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	EnvironmentID  uuid.UUID  `json:"environment_id"`
	SourceType     string     `json:"source_type"`
	SourceID       uuid.UUID  `json:"source_id"`
	RunID          uuid.UUID  `json:"run_id"`
	AttemptNumber  *int32     `json:"attempt_number,omitempty"`
	TraceID        string     `json:"trace_id"`
	SpanID         string     `json:"span_id"`
	Meter          string     `json:"meter"`
	Quantity       string     `json:"quantity"`
	Unit           string     `json:"unit"`
	MeasuredTo     *time.Time `json:"measured_to,omitempty"`
	Details        string     `json:"details"`
	IdempotencyKey string     `json:"idempotency_key"`
	OccurredAt     time.Time  `json:"occurred_at"`
	CreatedAt      time.Time  `json:"created_at"`
}
