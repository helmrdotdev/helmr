package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"time"
	"uuid"
)

func (r *Reader) ListCommandLogChunks(ctx context.Context, q telemetry.CommandLogChunkQuery) (telemetry.CommandLogChunkPage, error) {
	if q.OrgID == uuid.Nil() || q.EnvironmentID == uuid.Nil() || q.CommandID == uuid.Nil() ||
		(q.Stream != "stdout" && q.Stream != "stderr") || q.Limit < 1 || q.Limit > 64 {
		return telemetry.CommandLogChunkPage{}, errors.New("invalid exec log query")
	}
	var after uint64
	if q.AfterObservedSeq != nil {
		after = *q.AfterObservedSeq
	}
	var rows []commandLogRow
	err := r.client.Select(ctx, &rows, `SELECT sequence, through_sequence, kind, data, observed_at_unix_nano, dropped_bytes, complete, accepted_at, expires_at
FROM helmr_telemetry.computer_command_logs FINAL
WHERE environment_id = @environment_id
  AND command_id = @command_id
  AND producer_epoch = 1
  AND stream = @stream
  AND (@has_after = 0 OR sequence > @after)
  AND expires_at > now64(6)
ORDER BY sequence ASC
LIMIT @row_limit`,
		Named("environment_id", q.EnvironmentID), Named("command_id", q.CommandID),
		Named("stream", q.Stream), Named("has_after", q.AfterObservedSeq != nil), Named("after", after), Named("row_limit", uint32(q.Limit)))
	if err != nil {
		return telemetry.CommandLogChunkPage{}, fmt.Errorf("%w: %v", telemetry.ErrHistoricalUnavailable, err)
	}
	page := telemetry.CommandLogChunkPage{Chunks: make([]telemetry.CommandLogChunk, 0, len(rows))}
	for _, row := range rows {
		page.Chunks = append(page.Chunks, telemetry.CommandLogChunk{
			Kind: row.Kind, ThroughSequence: uint64(row.ThroughSequence), DroppedBytes: row.DroppedBytes, Complete: row.Complete, ExpiresAt: row.ExpiresAt, ObservedSeq: uint64(row.Sequence), Content: []byte(row.Data), ObservedAt: time.Unix(0, row.ObservedAtUnixNano).UTC(), AcceptedAt: row.AcceptedAt,
		})
	}
	return page, nil
}

type commandLogRow struct {
	Sequence           int64     `ch:"sequence"`
	ThroughSequence    int64     `ch:"through_sequence"`
	Kind               string    `ch:"kind"`
	Data               string    `ch:"data"`
	ObservedAtUnixNano int64     `ch:"observed_at_unix_nano"`
	DroppedBytes       int64     `ch:"dropped_bytes"`
	Complete           bool      `ch:"complete"`
	AcceptedAt         time.Time `ch:"accepted_at"`
	ExpiresAt          time.Time `ch:"expires_at"`
}
