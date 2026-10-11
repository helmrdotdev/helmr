package telemetry

import (
	"context"
	"time"
	"uuid"
)

type CommandLogReader interface {
	ListCommandLogChunks(context.Context, CommandLogChunkQuery) (CommandLogChunkPage, error)
}

type CommandLogChunkQuery struct {
	OrgID            uuid.UUID
	EnvironmentID    uuid.UUID
	CommandID        uuid.UUID
	Stream           string
	AfterObservedSeq *uint64
	Limit            int32
}

type CommandLogChunk struct {
	Kind            string
	ThroughSequence uint64
	DroppedBytes    int64
	Complete        bool
	ExpiresAt       time.Time
	ObservedSeq     uint64
	Content         []byte
	ObservedAt      time.Time
	AcceptedAt      time.Time
}

type CommandLogChunkPage struct {
	Chunks []CommandLogChunk
}
