//go:build linux && computerproof

package controlplane

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The isolated fixture retains accepted diagnostic chunks in its disposable
// database. It has no historical telemetry service or export loop.
type nativeSDKCommandLogReader struct{ pool *pgxpool.Pool }

func (r nativeSDKCommandLogReader) ListCommandLogChunks(ctx context.Context, q telemetry.CommandLogChunkQuery) (telemetry.CommandLogChunkPage, error) {
	var after uint64
	if q.AfterObservedSeq != nil {
		after = *q.AfterObservedSeq
	}
	rows, err := r.pool.Query(ctx, `SELECT d.kind,d.through_sequence,d.dropped_bytes,d.complete,d.sequence,d.data,d.observed_at_unix_nano,d.accepted_at
 FROM diagnostic_chunks d JOIN environments e ON e.id=d.environment_id
 WHERE e.org_id=$1 AND d.environment_id=$2 AND d.command_id=$3 AND d.stream=$4 AND d.sequence>$5
 ORDER BY d.sequence LIMIT $6`, q.OrgID.String(), q.EnvironmentID.String(), q.CommandID.String(), q.Stream, after, q.Limit)
	if err != nil {
		return telemetry.CommandLogChunkPage{}, err
	}
	defer rows.Close()
	result := telemetry.CommandLogChunkPage{}
	for rows.Next() {
		var chunk telemetry.CommandLogChunk
		var observed int64
		if err := rows.Scan(&chunk.Kind, &chunk.ThroughSequence, &chunk.DroppedBytes, &chunk.Complete, &chunk.ObservedSeq, &chunk.Content, &observed, &chunk.AcceptedAt); err != nil {
			return telemetry.CommandLogChunkPage{}, err
		}
		chunk.ObservedAt = time.Unix(0, observed)
		chunk.ExpiresAt = chunk.AcceptedAt.Add(telemetry.DiagnosticRetention)
		result.Chunks = append(result.Chunks, chunk)
	}
	return result, rows.Err()
}
