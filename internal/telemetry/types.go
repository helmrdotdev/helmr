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
	OrgID       uuid.UUID
	CellID      string
	SubjectType string
	SubjectID   uuid.UUID
	AfterSeq    int64
	Limit       int32
}

type EventPage struct {
	Events     []api.RunEvent
	LastSeq    int64
	Watermark  int64
	HotCount   int
	Historical int
}

type RunLogChunkQuery struct {
	OrgID    uuid.UUID
	CellID   string
	RunID    uuid.UUID
	AfterSeq int64
	Limit    int32
}

type RunLogChunkPage struct {
	Chunks     []api.RunLogChunk
	LastSeq    int64
	Watermark  int64
	HotCount   int
	Historical int
}

type RunLogSnapshotQuery struct {
	OrgID       uuid.UUID
	CellID      string
	RunID       uuid.UUID
	StdoutLimit int64
	StderrLimit int64
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
	OrgID      uuid.UUID
	CellID     string
	StreamKind string
	SourceKind string
	SourceID   uuid.UUID
	AfterSeq   int64
	Watermark  int64
}

type TerminalOutputQuery struct {
	OrgID         uuid.UUID
	CellID        string
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
	WriteTerminalOutput(context.Context, []TerminalOutputRecord) error
}
