package telemetry

import (
	"context"
	"errors"
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

type IngestWriter interface {
	WriteEvents(context.Context, []EventRecord) error
	WriteRunLogs(context.Context, []RunLogRecord) error
}
