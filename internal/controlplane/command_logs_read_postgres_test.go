package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type commandHistoryReader struct {
	telemetry.CommandLogReader
	pipes map[string][]telemetry.CommandLogChunk
	fail  error
}

func (r *commandHistoryReader) ListCommandLogChunks(_ context.Context, q telemetry.CommandLogChunkQuery) (telemetry.CommandLogChunkPage, error) {
	if r.fail != nil {
		return telemetry.CommandLogChunkPage{}, r.fail
	}
	page := telemetry.CommandLogChunkPage{}
	for _, row := range r.pipes[q.Stream] {
		if q.AfterObservedSeq == nil || row.ObservedSeq > *q.AfterObservedSeq {
			page.Chunks = append(page.Chunks, row)
		}
		if len(page.Chunks) == int(q.Limit) {
			break
		}
	}
	return page, nil
}

func TestRunningCommandOutputDoesNotWaitForHistory(t *testing.T) {
	f, worker, target := commandHTTPFixture(t)
	worker.post(t, "/worker/v1/computer-commands/claim", workerapi.ComputerCommandClaimRequest{EnvironmentID: f.EnvironmentID.String(), AllocationID: target.InstanceID.String(), WriterGeneration: 1}, http.StatusOK, nil)
	worker.post(t, "/worker/v1/computer-commands/logs/append", workerapi.CommandLogAppendRequest{EnvironmentID: f.EnvironmentID.String(), CommandID: target.ID.String(), AllocationID: target.InstanceID.String(), WriterGeneration: 1, Stream: "stdout", Kind: "data", ObservedSeq: 1, ThroughSequence: 1, Content: []byte("ready"), ObservedAt: time.Now().UTC()}, http.StatusOK, nil)
	q := db.New(f.Pool)
	command, err := q.GetCommandLogState(t.Context(), db.GetCommandLogStateParams{OrgID: pgvalue.UUID(f.OrgID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), CommandID: pgvalue.UUID(target.ID)})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(testAuthRootKey())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: q, tx: f.Pool, authKeys: keys, telemetryReader: &commandHistoryReader{fail: errors.New("historical sink unavailable")}, diagnosticBounds: completeServerConfig(t).DiagnosticBounds}
	page, err := server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, "", 10)
	if err != nil || len(page.Logs) != 1 || page.Logs[0].ContentBase64 != base64.StdEncoding.EncodeToString([]byte("ready")) || page.OutputState != "open" {
		t.Fatalf("running stdout with silent stderr: %+v %v", page, err)
	}
	page, err = server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, page.NextCursor, 10)
	if err != nil || len(page.Logs) != 0 || page.OutputState != "open" {
		t.Fatalf("caught-up live cursor consulted history: %+v %v", page, err)
	}
}

func TestCommandHistoryUsesExplicitCoverageAndAcceptanceRetention(t *testing.T) {
	f, worker, target := commandHTTPFixture(t)
	claim := workerapi.ComputerCommandClaimRequest{EnvironmentID: f.EnvironmentID.String(), AllocationID: target.InstanceID.String(), WriterGeneration: 1}
	worker.post(t, "/worker/v1/computer-commands/claim", claim, http.StatusOK, nil)
	binary := []byte{0, 255, 128, 10}
	now := time.Now().UTC().Truncate(time.Microsecond)
	rows := map[string][]telemetry.CommandLogChunk{
		"stdout": {
			{Kind: "data", ObservedSeq: 1, ThroughSequence: 1, Content: binary, ObservedAt: now},
			{Kind: "gap", ObservedSeq: 2, ThroughSequence: 3, DroppedBytes: 10, ObservedAt: now.Add(time.Nanosecond)},
			{Kind: "end", ObservedSeq: 4, ThroughSequence: 4, Complete: true, ObservedAt: now.Add(2 * time.Nanosecond)},
		},
		"stderr": {{Kind: "end", ObservedSeq: 1, ThroughSequence: 1, Complete: true, ObservedAt: now.Add(3 * time.Nanosecond)}},
	}
	for stream, pipe := range rows {
		for i, row := range pipe {
			var receipt workerapi.DiagnosticLogReceipt
			request := workerapi.CommandLogAppendRequest{EnvironmentID: f.EnvironmentID.String(), CommandID: target.ID.String(), AllocationID: target.InstanceID.String(), WriterGeneration: 1, Stream: workerapi.LogStream(stream), Kind: row.Kind, ObservedSeq: row.ObservedSeq, ThroughSequence: row.ThroughSequence, DroppedBytes: row.DroppedBytes, Complete: row.Complete, Content: row.Content, ObservedAt: row.ObservedAt}
			worker.post(t, "/worker/v1/computer-commands/logs/append", request, http.StatusOK, &receipt)
			rows[stream][i].AcceptedAt, rows[stream][i].ExpiresAt = receipt.AcceptedAt, receipt.ExpiresAt
		}
	}
	code := int32(0)
	worker.post(t, "/worker/v1/computer-commands/complete", workerapi.ComputerCommandCompleteRequest{EnvironmentID: f.EnvironmentID.String(), CommandID: target.ID.String(), AllocationID: target.InstanceID.String(), WriterGeneration: 1, Outcome: "exited", ExitCode: &code, Stdout: workerapi.CommandOutputBoundary{ThroughSequence: 4, Complete: true, Gapped: true}, Stderr: workerapi.CommandOutputBoundary{ThroughSequence: 1, Complete: true}}, http.StatusOK, nil)
	// A command's creation clock must not expire newly accepted output.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE commands SET created_at=created_at-interval '91 days' WHERE id=$1`, target.ID)
	q := db.New(f.Pool)
	command, err := q.GetCommandLogState(t.Context(), db.GetCommandLogStateParams{OrgID: pgvalue.UUID(f.OrgID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), CommandID: pgvalue.UUID(target.ID)})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(testAuthRootKey())
	if err != nil {
		t.Fatal(err)
	}
	history := &commandHistoryReader{pipes: map[string][]telemetry.CommandLogChunk{}}
	server := &Server{db: q, tx: f.Pool, authKeys: keys, telemetryReader: history, diagnosticBounds: completeServerConfig(t).DiagnosticBounds}
	// Committed output and end records are visible without waiting for export.
	pending, err := server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, "", 10)
	if err != nil || len(pending.Logs) != 2 || pending.OutputState != "closed" {
		t.Fatalf("unexported output %+v %v", pending, err)
	}
	// A page can begin in exported history and continue in the pending queue.
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM diagnostic_chunks WHERE command_id=$1 AND stream='stdout' AND sequence=1`, target.ID)
	history.pipes = map[string][]telemetry.CommandLogChunk{"stdout": rows["stdout"][:1]}
	pending, err = server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, "", 10)
	if err != nil || len(pending.Logs) != 2 || pending.OutputState != "closed" {
		t.Fatalf("export frontier %+v %v", pending, err)
	}
	// Retired queue entries continue to use the historical retention contract.
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM diagnostic_chunks WHERE command_id=$1`, target.ID)
	history.pipes = rows
	page, err := server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, "", 1)
	if err != nil || len(page.Logs) != 1 || page.Logs[0].Kind != "output" || page.OutputState != "open" {
		t.Fatalf("first %+v %v", page, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(page.Logs[0].ContentBase64)
	if err != nil || !bytes.Equal(decoded, binary) {
		t.Fatal("binary bytes changed")
	}
	page, err = server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, page.NextCursor, 1)
	if err != nil || len(page.Logs) != 1 || page.Logs[0].Kind != "gap" || page.Logs[0].FromSequence != "2" || page.Logs[0].ThroughSequence != "3" {
		t.Fatalf("gap %+v %v", page, err)
	}
	afterGap := page.NextCursor
	page, err = server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, afterGap, 1)
	if err != nil || len(page.Logs) != 0 || page.NextCursor != "" || page.OutputState != "closed" {
		t.Fatalf("end %+v %v", page, err)
	}
	history.pipes = map[string][]telemetry.CommandLogChunk{"stdout": rows["stdout"][1:], "stderr": rows["stderr"]}
	_, err = server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, "", 10)
	var lag telemetry.LaggingError
	if !errors.As(err, &lag) {
		t.Fatalf("missing history fabricated a gap or EOF: %v", err)
	}
	history.pipes = map[string][]telemetry.CommandLogChunk{"stdout": rows["stdout"][:2], "stderr": rows["stderr"]}
	_, err = server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, afterGap, 10)
	if !errors.As(err, &lag) {
		t.Fatalf("missing end fabricated closure: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE commands SET stdout_last_accepted_at=stdout_last_accepted_at-interval '91 days' WHERE id=$1`, target.ID)
	_, err = server.readCommandLogs(t.Context(), pgvalue.UUID(f.OrgID), command, "", 10)
	if errorStatus(err) != http.StatusGone {
		t.Fatalf("expired acceptance: %v", err)
	}

}
