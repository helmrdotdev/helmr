package control

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace/protocol"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkspacePrimitiveOperationFingerprintMatchesGuestdContract(t *testing.T) {
	request := []byte(`{"exec_id":"exec-1","command":["echo","ok"]}`)
	got, err := workspacePrimitiveOperationFingerprint(workspaceOperationKindStartExec, request)
	if err != nil {
		t.Fatal(err)
	}
	want, err := protocol.RequestFingerprint("StartExec", request)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
}

func TestWorkspacePrimitiveOperationFingerprintIgnoresJSONRepresentation(t *testing.T) {
	created, err := workspacePrimitiveOperationFingerprint(workspaceOperationKindStartExec, []byte(`{"exec_id":"exec-1","command":["echo","ok"],"detached":false}`))
	if err != nil {
		t.Fatal(err)
	}
	transported, err := protocol.RequestFingerprint("StartExec", []byte(`{ "detached": false, "command": [ "echo", "ok" ], "exec_id": "exec-1" }`))
	if err != nil {
		t.Fatal(err)
	}
	if created != transported {
		t.Fatalf("fingerprints differ after JSON re-encoding: %s != %s", created, transported)
	}
}

func TestWorkspaceExecTerminalEventMatchesOnlySamePayload(t *testing.T) {
	materializationID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	row := db.WorkspaceExec{
		MaterializationID: materializationID,
		State:             db.WorkspaceExecStateExited,
		ExitCode:          pgtype.Int4{Int32: 7, Valid: true},
		Signal:            "",
		Error:             []byte(`{"message":"done"}`),
	}
	if !workspaceExecTerminalEventMatches(row, materializationID, db.WorkspaceExecStateExited, pgtype.Int4{Int32: 7, Valid: true}, "", []byte(`{"message":"done"}`)) {
		t.Fatal("same exec terminal event did not match")
	}
	if workspaceExecTerminalEventMatches(row, materializationID, db.WorkspaceExecStateExited, pgtype.Int4{Int32: 8, Valid: true}, "", []byte(`{"message":"done"}`)) {
		t.Fatal("different exit code matched")
	}
	if workspaceExecTerminalEventMatches(row, materializationID, db.WorkspaceExecStateFailed, pgtype.Int4{Int32: 7, Valid: true}, "", []byte(`{"message":"done"}`)) {
		t.Fatal("different terminal state matched")
	}
	if workspaceExecTerminalEventMatches(row, pgvalue.UUID(uuid.Must(uuid.NewV7())), db.WorkspaceExecStateExited, pgtype.Int4{Int32: 7, Valid: true}, "", []byte(`{"message":"done"}`)) {
		t.Fatal("different materialization matched")
	}
	lost := db.WorkspaceExec{
		MaterializationID: materializationID,
		State:             db.WorkspaceExecStateLost,
	}
	if !workspaceExecTerminalEventMatches(lost, materializationID, db.WorkspaceExecStateExited, pgtype.Int4{Int32: 0, Valid: true}, "", nil) {
		t.Fatal("late exec terminal event did not match lost row")
	}
}

func TestWorkspacePtyLifecycleEventMatchesOnlySamePayload(t *testing.T) {
	materializationID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	resized := db.WorkspacePtySession{
		MaterializationID: materializationID,
		State:             db.WorkspacePtyStateOpen,
		Cols:              120,
		Rows:              40,
	}
	if !workspacePtyResizeAppliedEventMatches(resized, materializationID, 120, 40) {
		t.Fatal("same pty resize-applied event did not match")
	}
	if workspacePtyResizeAppliedEventMatches(resized, materializationID, 80, 24) {
		t.Fatal("different pty size matched")
	}
	failed := db.WorkspacePtySession{
		MaterializationID: materializationID,
		State:             db.WorkspacePtyStateFailed,
		Error:             []byte(`{"message":"closed"}`),
	}
	if !workspacePtyTerminalEventMatches(failed, materializationID, true, []byte(`{"message":"closed"}`)) {
		t.Fatal("same failed pty event did not match")
	}
	if workspacePtyTerminalEventMatches(failed, materializationID, true, []byte(`{"message":"other"}`)) {
		t.Fatal("different pty error matched")
	}
	closed := db.WorkspacePtySession{
		MaterializationID: materializationID,
		State:             db.WorkspacePtyStateClosed,
	}
	if !workspacePtyTerminalEventMatches(closed, materializationID, false, nil) {
		t.Fatal("same closed pty event did not match")
	}
	if workspacePtyTerminalEventMatches(closed, pgvalue.UUID(uuid.Must(uuid.NewV7())), false, nil) {
		t.Fatal("different materialization matched")
	}
	lost := db.WorkspacePtySession{
		MaterializationID: materializationID,
		State:             db.WorkspacePtyStateLost,
	}
	if !workspacePtyTerminalEventMatches(lost, materializationID, false, nil) {
		t.Fatal("late pty closed event did not match lost row")
	}
	if !workspacePtyTerminalEventMatches(lost, materializationID, true, []byte(`{"message":"late"}`)) {
		t.Fatal("late pty error event did not match lost row")
	}
}

func TestWorkspacePtyResizeAppliedEventMatchesCloseRace(t *testing.T) {
	materializationID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	row := db.WorkspacePtySession{
		MaterializationID: materializationID,
		State:             db.WorkspacePtyStateClosing,
		Cols:              100,
		Rows:              30,
	}
	if !workspacePtyResizeAppliedEventMatches(row, materializationID, 100, 30) {
		t.Fatal("resize-applied close race did not match")
	}
	if workspacePtyResizeAppliedEventMatches(row, materializationID, 101, 30) {
		t.Fatal("different resize dimensions matched")
	}
	if workspacePtyResizeAppliedEventMatches(row, pgvalue.UUID(uuid.Must(uuid.NewV7())), 100, 30) {
		t.Fatal("different materialization matched")
	}
}

func TestWorkspacePtyCreatingRejectsResizeAndClose(t *testing.T) {
	pty := db.WorkspacePtySession{State: db.WorkspacePtyStateCreating}
	server := &Server{}
	if _, err := server.requestWorkspacePtyResize(context.Background(), pty, 120, 40); !isConflict(err, errWorkspacePtyNotOpen) {
		t.Fatalf("resize creating pty err = %v, want workspace_pty_not_open conflict", err)
	}
	if _, err := server.requestWorkspacePtyCloseOperation(context.Background(), pty); !isConflict(err, errWorkspacePtyNotOpen) {
		t.Fatalf("close creating pty err = %v, want workspace_pty_not_open conflict", err)
	}
}

func TestWorkspacePtyClosingRejectsResizeAndAcceptsCloseRetry(t *testing.T) {
	pty := db.WorkspacePtySession{State: db.WorkspacePtyStateClosing}
	server := &Server{}
	if _, err := server.requestWorkspacePtyResize(context.Background(), pty, 120, 40); !isConflict(err, errWorkspacePtyNotOpen) {
		t.Fatalf("resize closing pty err = %v, want workspace_pty_not_open conflict", err)
	}
	if _, err := server.requestWorkspacePtyCloseOperation(context.Background(), pty); err == nil || strings.Contains(err.Error(), "not open") {
		t.Fatalf("close closing pty err = %v, want close retry to pass application state guard", err)
	}
}

func TestNormalizeWorkspaceExecStateFilterRejectsUnknownState(t *testing.T) {
	if got, err := normalizeWorkspaceExecStateFilter("running"); err != nil || got != db.WorkspaceExecStateRunning {
		t.Fatalf("running state = %s err=%v", got, err)
	}
	if _, err := normalizeWorkspaceExecStateFilter("bogus"); err == nil || !strings.Contains(err.Error(), "state must be one of") {
		t.Fatalf("bogus state err = %v", err)
	}
}

func TestWorkspaceWorkerOutputAppendRequiresExplicitOffset(t *testing.T) {
	server := &Server{}
	if _, err := server.appendWorkspaceExecOutputStreamChunk(context.Background(), db.WorkspaceExec{}, db.WorkspaceExecStreamStdout, nil, []byte("x")); !isBadRequest(err) {
		t.Fatalf("exec output nil offset err = %v, want bad request", err)
	}
	if _, err := server.appendWorkspacePtyOutputStreamChunk(context.Background(), db.WorkspacePtySession{}, nil, []byte("x")); !isBadRequest(err) {
		t.Fatalf("pty output nil offset err = %v, want bad request", err)
	}
}

func isConflict(err error, target error) bool {
	var apiErr apiError
	return errors.As(err, &apiErr) && apiErr.kind == errConflict && errors.Is(err, target)
}

func isBadRequest(err error) bool {
	var apiErr apiError
	return errors.As(err, &apiErr) && apiErr.kind == errBadRequest
}
