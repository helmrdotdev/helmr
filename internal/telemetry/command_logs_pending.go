package telemetry

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

// PendingCommandLogs reads committed output before export. Callers read this
// before the historical sink, so export retirement between the two reads does
// not remove the only copy they can observe. This is the existing bounded export
// queue, not a second retention store.
func PendingCommandLogs(ctx context.Context, database db.DBTX, query CommandLogChunkQuery) (CommandLogChunkPage, error) {
	var after uint64
	if query.AfterObservedSeq != nil {
		after = *query.AfterObservedSeq
	}
	rows, err := database.Query(ctx, `SELECT o.kind,o.sequence,o.through_sequence,o.observed_at_unix_nano,o.data,o.dropped_bytes,o.complete,o.accepted_at
 FROM diagnostic_chunks o JOIN environments e ON e.id=o.environment_id
 WHERE e.org_id=$1 AND o.environment_id=$2 AND o.source_kind='computer_command'
 AND o.source_id=$3 AND o.producer_epoch=1 AND o.stream=$4 AND o.sequence>$5
 AND o.accepted_at>statement_timestamp()-interval '2160 hours'
 ORDER BY o.sequence LIMIT $6`, query.OrgID, query.EnvironmentID, query.CommandID, query.Stream, after, query.Limit)
	if err != nil {
		return CommandLogChunkPage{}, err
	}
	defer rows.Close()
	var page CommandLogChunkPage
	for rows.Next() {
		var chunk CommandLogChunk
		var observed int64
		if err := rows.Scan(&chunk.Kind, &chunk.ObservedSeq, &chunk.ThroughSequence, &observed, &chunk.Content, &chunk.DroppedBytes, &chunk.Complete, &chunk.AcceptedAt); err != nil {
			return CommandLogChunkPage{}, err
		}
		chunk.ObservedAt = time.Unix(0, observed).UTC()
		chunk.ExpiresAt = chunk.AcceptedAt.Add(DiagnosticRetention)
		page.Chunks = append(page.Chunks, chunk)
	}
	return page, rows.Err()
}
