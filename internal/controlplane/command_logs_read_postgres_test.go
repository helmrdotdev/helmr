package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	commandowner "github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5/pgtype"
)

type commandLogTestReader struct {
	telemetry.Reader
	chunks map[string][]telemetry.CommandLogChunk
}

func (r *commandLogTestReader) ListCommandLogChunks(_ context.Context, q telemetry.CommandLogChunkQuery) (telemetry.CommandLogChunkPage, error) {
	page := telemetry.CommandLogChunkPage{}
	for _, chunk := range r.chunks[q.Stream] {
		if q.AfterObservedSeq == nil || chunk.ObservedSeq > *q.AfterObservedSeq {
			page.Chunks = append(page.Chunks, chunk)
			if len(page.Chunks) == int(q.Limit) {
				break
			}
		}
	}
	return page, nil
}

func TestCommandLogsReadDeliveryCursorAndExpiry(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	command, err := commandowner.Create(t.Context(), f.pool, commandowner.CreateRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, ComputerID: f.computerIDs[0], Creator: commandowner.Creator{SubjectType: "api_key", SubjectID: uuid.NewV7().String()}, Argv: []string{"true"}, IdempotencyKey: "read-logs"})
	if err != nil {
		t.Fatal(err)
	}
	f.server.authKeys = auth.Keys{TelemetryCursor: make([]byte, auth.RootKeySize)}
	sink := &commandLogTestReader{chunks: map[string][]telemetry.CommandLogChunk{}}
	f.server.telemetryReader = sink
	at := time.Now().UTC().Truncate(time.Microsecond)
	appendChunk := func(stream string, seq int64) {
		t.Helper()
		_, err := f.server.db.InsertCommandLogChunk(t.Context(), db.InsertCommandLogChunkParams{OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID), EnvironmentID: command.EnvironmentID, CommandID: command.ID, StreamName: stream, Content: []byte{0, 255}, ObservedSeq: seq, ObservedAt: pgtype.Timestamptz{Time: at, Valid: true}})
		if err != nil {
			t.Fatal(err)
		}
	}
	markWritten := func(stream string, seq int64) {
		t.Helper()
		if _, err := f.pool.Exec(t.Context(), `UPDATE telemetry_outbox SET status='written', written_at=now() WHERE command_id=$1 AND stream_name=$2 AND observed_seq=$3`, command.ID, stream, seq); err != nil {
			t.Fatal(err)
		}
	}
	read := func(cursor string) (int, string, error) {
		page, err := f.server.readCommandLogs(t.Context(), pgvalue.UUID(f.orgID), command, cursor, 10)
		return len(page.Logs), page.NextCursor, err
	}
	page, err := f.server.readCommandLogs(t.Context(), pgvalue.UUID(f.orgID), command, "", 10)
	if err != nil || len(page.Logs) != 0 || page.OutputState != "open" {
		t.Fatalf("initial page=%+v %v", page, err)
	}
	appendChunk("stdout", 0)
	var lag telemetry.LaggingError
	if _, _, err := read(""); !errors.As(err, &lag) {
		t.Fatalf("pending empty tail=%v", err)
	}
	sink.chunks["stdout"] = []telemetry.CommandLogChunk{{ObservedSeq: 0, Content: []byte{0, 255}, ObservedAt: at}}
	markWritten("stdout", 0)
	// The other stream's pending row must not prevent delivery of a safe prefix.
	appendChunk("stderr", 0)
	count, cursor, err := read("")
	if err != nil || count != 1 || cursor == "" {
		t.Fatalf("safe prefix=%d %s %v", count, cursor, err)
	}
	if _, _, err = read(cursor); !errors.As(err, &lag) {
		t.Fatalf("pending stderr=%v", err)
	}
	sink.chunks["stderr"] = []telemetry.CommandLogChunk{{ObservedSeq: 0, Content: []byte("err"), ObservedAt: at}}
	markWritten("stderr", 0)
	count, next, err := read(cursor)
	if err != nil || count != 1 || next == cursor {
		t.Fatalf("reconnect=%d %s %v", count, next, err)
	}
	if n, _, err := read(next); err != nil || n != 0 {
		t.Fatalf("drained running page=%d %v", n, err)
	}
	wrong := command
	wrong.ID = pgvalue.UUID(uuid.NewV7())
	if _, err := f.server.readCommandLogs(t.Context(), pgvalue.UUID(f.orgID), wrong, next, 10); !errors.Is(err, errTelemetryInvalidCursor) {
		t.Fatalf("wrong command cursor=%v", err)
	}
	if _, _, err = read(next + "x"); !errors.Is(err, errTelemetryInvalidCursor) {
		t.Fatalf("tampered cursor=%v", err)
	}
	command.CreatedAt = pgtype.Timestamptz{Time: at.Add(-91 * 24 * time.Hour), Valid: true}
	sink.chunks = map[string][]telemetry.CommandLogChunk{}
	if _, _, err = read(""); err == nil {
		t.Fatal("expired history must not become an empty successful history")
	}
}

func TestCommandLogsGapWaitsUntilProducerIsFenced(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	f.server.authKeys = auth.Keys{TelemetryCursor: make([]byte, auth.RootKeySize)}
	now := time.Now().UTC()
	command := db.ComputerCommand{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.environmentID), CreatedAt: pgtype.Timestamptz{Time: now, Valid: true}}
	f.server.telemetryReader = &commandLogTestReader{chunks: map[string][]telemetry.CommandLogChunk{"stdout": {{ObservedSeq: 2, Content: []byte("tail"), ObservedAt: now}}}}
	var lag telemetry.LaggingError
	if _, err := f.server.readCommandLogs(t.Context(), pgvalue.UUID(f.orgID), command, "", 1); !errors.As(err, &lag) {
		t.Fatalf("live missing sequence=%v", err)
	}
	command.TerminalAt = pgtype.Timestamptz{Time: now, Valid: true}
	page, err := f.server.readCommandLogs(t.Context(), pgvalue.UUID(f.orgID), command, "", 1)
	if err != nil || len(page.Logs) != 1 || page.Logs[0].Kind != "gap" || page.Logs[0].FromSequence != "0" || page.Logs[0].ThroughSequence != "1" {
		t.Fatalf("gap=%+v %v", page, err)
	}
	tail, err := f.server.readCommandLogs(t.Context(), pgvalue.UUID(f.orgID), command, page.NextCursor, 1)
	if err != nil || len(tail.Logs) != 1 || tail.Logs[0].Kind != "output" {
		t.Fatalf("tail=%+v %v", tail, err)
	}
}

func TestCommandOutputClosureDistinguishesForcedPipeLoss(t *testing.T) {
	command := db.ComputerCommand{ComputerInstanceID: pgvalue.UUID(uuid.NewV7()), TerminalAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	for _, test := range []struct{ reason, want string }{
		{"computer_command_completed", "closed"},
		{"computer_command_timed_out", "closed"},
		{"computer_command_signaled", "closed"},
		{"computer_command_scope_termination_failed", "unavailable"},
		{"computer_command_output_capture_failed", "unavailable"},
		{"computer_command_worker_lost", "unavailable"},
	} {
		command.TerminalReasonCode = pgvalue.Text(test.reason)
		if got := commandOutputState(command); got != test.want {
			t.Errorf("%s output=%s, want %s", test.reason, got, test.want)
		}
	}
}
