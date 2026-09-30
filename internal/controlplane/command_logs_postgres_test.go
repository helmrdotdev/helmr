package controlplane

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCommandLogOutboxScopeReplayAndCollection(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	admitted, err := command.Create(t.Context(), f.pool, command.CreateRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, ComputerID: f.computerIDs[0], Creator: command.Creator{SubjectType: "api_key", SubjectID: uuid.NewV7().String()}, Argv: []string{"true"}, IdempotencyKey: "log-test"})
	if err != nil {
		t.Fatal(err)
	}
	q := f.server.db
	p := db.InsertCommandLogChunkParams{OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID), EnvironmentID: pgvalue.UUID(f.environmentID), CommandID: admitted.ID, StreamName: "stdout", Content: []byte{0, 255, 128}, ObservedSeq: 0, ObservedAt: pgtype.Timestamptz{Time: time.Now().UTC().Truncate(time.Microsecond), Valid: true}}
	first, err := q.InsertCommandLogChunk(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := q.InsertCommandLogChunk(t.Context(), p)
	if err != nil || replay.ID != first.ID || !replay.CreatedAt.Time.Equal(first.CreatedAt.Time) {
		t.Fatalf("replay=%+v, %v", replay, err)
	}
	changed := p
	changed.Content = []byte("changed")
	if _, err := q.InsertCommandLogChunk(t.Context(), changed); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("changed replay=%v", err)
	}
	wrong := p
	wrong.EnvironmentID = pgvalue.UUID(uuid.NewV7())
	if _, err := q.InsertCommandLogChunk(t.Context(), wrong); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong scope=%v", err)
	}
	claim := db.ClaimCommandLogIngestBatchParams{RowLimit: 100, MaxBatchBytes: 2, LeaseDuration: pgvalue.Interval(time.Minute)}
	if rows, err := q.ClaimCommandLogIngestBatch(t.Context(), claim); err != nil || len(rows) != 0 {
		t.Fatalf("byte budget=%v, %v", rows, err)
	}
	claim.MaxBatchBytes = 3
	rows, err := q.ClaimCommandLogIngestBatch(t.Context(), claim)
	if err != nil || len(rows) != 1 || rows[0].CommandID != admitted.ID {
		t.Fatalf("claim=%v, %v", rows, err)
	}
	if count, err := q.PruneTelemetryOutboxWritten(t.Context(), db.PruneTelemetryOutboxWrittenParams{RetainFor: pgvalue.Interval(0), RowLimit: 100}); err != nil || count != 0 {
		t.Fatalf("premature prune=%d, %v", count, err)
	}
	if count, err := q.MarkTelemetryOutboxWritten(t.Context(), db.MarkTelemetryOutboxWrittenParams{Ids: []int64{first.ID}, ExpectedRetryCounts: []int32{rows[0].RetryCount}}); err != nil || count != 1 {
		t.Fatalf("ack=%d, %v", count, err)
	}
	if count, err := q.PruneTelemetryOutboxWritten(t.Context(), db.PruneTelemetryOutboxWrittenParams{RetainFor: pgvalue.Interval(0), RowLimit: 100}); err != nil || count != 0 {
		t.Fatalf("live execution prune=%d, %v", count, err)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE computer_commands SET status = 'failed', failure_reason='dispatch_failed', terminal_at = now(), terminal_reason_code = 'cancelled', result_expires_at = now() + interval '30 days' WHERE id = $1", admitted.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := q.PruneTelemetryOutboxWritten(t.Context(), db.PruneTelemetryOutboxWrittenParams{RetainFor: pgvalue.Interval(0), RowLimit: 100}); err != nil || count != 1 {
		t.Fatalf("prune=%d, %v", count, err)
	}
	for _, delivery := range []db.InsertCommandLogChunkParams{p, changed} {
		if _, err := q.InsertCommandLogChunk(t.Context(), delivery); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("terminal replay after collection=%v", err)
		}
	}
}
