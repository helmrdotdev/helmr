package telemetry

import (
	"context"
	"errors"
	"time"
	"uuid"

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
}

type EventQuery struct {
	OrgID       uuid.UUID
	SubjectType string
	SubjectID   uuid.UUID
	AfterSeq    int64
	Limit       int32
	Severities  []string
}

type EventPage struct {
	Events     []api.RunEvent
	LastSeq    int64
	Watermark  int64
	Historical int
}

type RunLogChunkQuery struct {
	OrgID    uuid.UUID
	RunID    uuid.UUID
	AfterSeq int64
	Limit    int32
	Levels   []string
}

type RunLogChunkPage struct {
	Chunks     []api.RunLogChunk
	LastSeq    int64
	Watermark  int64
	Historical int
}

type TerminalOutputQuery struct {
	OrgID         uuid.UUID
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
	OrgID          uuid.UUID  `json:"org_id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	EnvironmentID  uuid.UUID  `json:"environment_id"`
	SourceType     string     `json:"source_type"`
	SourceID       uuid.UUID  `json:"source_id"`
	RunID          *uuid.UUID `json:"run_id,omitempty"`
	DeploymentID   *uuid.UUID `json:"deployment_id,omitempty"`
	AttemptNumber  *int32     `json:"attempt_number,omitempty"`
	TraceID        string     `json:"trace_id"`
	SpanID         string     `json:"span_id"`
	Meter          string     `json:"meter"`
	Quantity       string     `json:"quantity"`
	Unit           string     `json:"unit"`
	MeasuredFrom   *time.Time `json:"measured_from,omitempty"`
	MeasuredTo     *time.Time `json:"measured_to,omitempty"`
	Details        string     `json:"details"`
	IdempotencyKey string     `json:"idempotency_key"`
	Fingerprint    string     `json:"idempotency_fingerprint"`
	OccurredAt     time.Time  `json:"occurred_at"`
	CreatedAt      time.Time  `json:"created_at"`
}
