package control

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type primitiveFailureStore struct {
	execExited []db.MarkWorkspaceExecExitedParams
	ptyFailed  []db.MarkWorkspacePtyFailedParams
}

func (s *primitiveFailureStore) MarkWorkspaceExecExited(_ context.Context, params db.MarkWorkspaceExecExitedParams) (db.MarkWorkspaceExecExitedRow, error) {
	s.execExited = append(s.execExited, params)
	return db.MarkWorkspaceExecExitedRow{}, nil
}

func (s *primitiveFailureStore) MarkWorkspacePtyFailed(_ context.Context, params db.MarkWorkspacePtyFailedParams) (db.MarkWorkspacePtyFailedRow, error) {
	s.ptyFailed = append(s.ptyFailed, params)
	return db.MarkWorkspacePtyFailedRow{}, nil
}

func TestFailWorkspacePrimitiveForOperationMarksStartExecFailed(t *testing.T) {
	store := &primitiveFailureStore{}
	failure := []byte(`{"code":"workspace_operation_dispatch_failed"}`)
	operation := testWorkspaceMaterializationOperation(workspaceOperationKindStartExec, workspaceOperationResourceExec)

	if err := failWorkspacePrimitiveForOperation(context.Background(), store, operation, failure); err != nil {
		t.Fatal(err)
	}
	if len(store.execExited) != 1 {
		t.Fatalf("exec failures = %d, want 1", len(store.execExited))
	}
	got := store.execExited[0]
	if got.State != db.WorkspaceExecStateFailed {
		t.Fatalf("exec state = %s, want failed", got.State)
	}
	if got.Error == nil || string(got.Error) != string(failure) {
		t.Fatalf("exec error = %s, want %s", got.Error, failure)
	}
	if got.ID != operation.ResourceID || got.MaterializationID != operation.MaterializationID {
		t.Fatalf("exec scope = %+v, want resource %v materialization %v", got, operation.ResourceID, operation.MaterializationID)
	}
	if got.ExitCode.Valid {
		t.Fatalf("exec exit_code valid = true, want false")
	}
	if len(store.ptyFailed) != 0 {
		t.Fatalf("pty failures = %d, want 0", len(store.ptyFailed))
	}
}

func TestFailWorkspacePrimitiveForOperationMarksCreatePtyFailed(t *testing.T) {
	store := &primitiveFailureStore{}
	failure := []byte(`{"code":"workspace_operation_dispatch_failed"}`)
	operation := testWorkspaceMaterializationOperation(workspaceOperationKindCreatePty, workspaceOperationResourcePty)

	if err := failWorkspacePrimitiveForOperation(context.Background(), store, operation, failure); err != nil {
		t.Fatal(err)
	}
	if len(store.ptyFailed) != 1 {
		t.Fatalf("pty failures = %d, want 1", len(store.ptyFailed))
	}
	got := store.ptyFailed[0]
	if got.Error == nil || string(got.Error) != string(failure) {
		t.Fatalf("pty error = %s, want %s", got.Error, failure)
	}
	if got.ID != operation.ResourceID || got.MaterializationID != operation.MaterializationID {
		t.Fatalf("pty scope = %+v, want resource %v materialization %v", got, operation.ResourceID, operation.MaterializationID)
	}
	if len(store.execExited) != 0 {
		t.Fatalf("exec failures = %d, want 0", len(store.execExited))
	}
}

func TestFailWorkspacePrimitiveForOperationIgnoresNonLifecycleOperations(t *testing.T) {
	store := &primitiveFailureStore{}
	operation := testWorkspaceMaterializationOperation(workspaceOperationKindResizePty, workspaceOperationResourcePty)

	if err := failWorkspacePrimitiveForOperation(context.Background(), store, operation, []byte(`{"code":"resize_failed"}`)); err != nil {
		t.Fatal(err)
	}
	if len(store.execExited) != 0 || len(store.ptyFailed) != 0 {
		t.Fatalf("primitive failures = exec %d pty %d, want none", len(store.execExited), len(store.ptyFailed))
	}
}

func TestFailWorkspacePrimitiveForOperationRejectsLifecycleResourceMismatch(t *testing.T) {
	store := &primitiveFailureStore{}
	operation := testWorkspaceMaterializationOperation(workspaceOperationKindStartExec, workspaceOperationResourcePty)

	if err := failWorkspacePrimitiveForOperation(context.Background(), store, operation, []byte(`{"code":"dispatch_failed"}`)); err == nil {
		t.Fatal("expected resource mismatch error")
	}
	if len(store.execExited) != 0 || len(store.ptyFailed) != 0 {
		t.Fatalf("primitive failures = exec %d pty %d, want none", len(store.execExited), len(store.ptyFailed))
	}
}

func testWorkspaceMaterializationOperation(kind string, resourceKind string) db.WorkspaceMaterializationOperation {
	return db.WorkspaceMaterializationOperation{
		OrgID:             testControlUUID(1),
		ProjectID:         testControlUUID(2),
		EnvironmentID:     testControlUUID(3),
		WorkspaceID:       testControlUUID(4),
		MaterializationID: testControlUUID(5),
		OperationKind:     kind,
		ResourceKind:      resourceKind,
		ResourceID:        testControlUUID(6),
	}
}

func testControlUUID(value byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{15: value}, Valid: true}
}
