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
	err := r.client.Select(ctx, &rows, `SELECT observed_seq, content, observed_at, accepted_at
FROM helmr_telemetry.command_logs FINAL
WHERE org_id = @org_id
  AND environment_id = @environment_id
  AND command_id = @command_id
  AND stream_name = @stream
  AND (@has_after = 0 OR observed_seq > @after)
  AND accepted_at >= now64(3) - INTERVAL 90 DAY
ORDER BY observed_seq ASC
LIMIT @row_limit`,
		Named("org_id", q.OrgID), Named("environment_id", q.EnvironmentID), Named("command_id", q.CommandID),
		Named("stream", q.Stream), Named("has_after", q.AfterObservedSeq != nil), Named("after", after), Named("row_limit", uint32(q.Limit)))
	if err != nil {
		return telemetry.CommandLogChunkPage{}, fmt.Errorf("%w: %v", telemetry.ErrHistoricalUnavailable, err)
	}
	page := telemetry.CommandLogChunkPage{Chunks: make([]telemetry.CommandLogChunk, 0, len(rows))}
	for _, row := range rows {
		page.Chunks = append(page.Chunks, telemetry.CommandLogChunk{
			ObservedSeq: row.ObservedSeq, Content: []byte(row.Content), ObservedAt: row.ObservedAt, AcceptedAt: row.AcceptedAt,
		})
	}
	return page, nil
}

type commandLogRow struct {
	ObservedSeq uint64    `ch:"observed_seq"`
	Content     string    `ch:"content"`
	ObservedAt  time.Time `ch:"observed_at"`
	AcceptedAt  time.Time `ch:"accepted_at"`
}
