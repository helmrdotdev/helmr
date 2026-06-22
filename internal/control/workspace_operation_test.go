package control

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type primitiveFailureStore struct {
	execExited          []db.MarkWorkspaceExecExitedParams
	ptyFailed           []db.MarkWorkspacePtyFailedParams
	ptyControlRollbacks []db.RollbackWorkspacePtyControlOperationParams
}

func (s *primitiveFailureStore) MarkWorkspaceExecExited(_ context.Context, params db.MarkWorkspaceExecExitedParams) (db.MarkWorkspaceExecExitedRow, error) {
	s.execExited = append(s.execExited, params)
	return db.MarkWorkspaceExecExitedRow{}, nil
}

func (s *primitiveFailureStore) MarkWorkspacePtyFailed(_ context.Context, params db.MarkWorkspacePtyFailedParams) (db.MarkWorkspacePtyFailedRow, error) {
	s.ptyFailed = append(s.ptyFailed, params)
	return db.MarkWorkspacePtyFailedRow{}, nil
}

func (s *primitiveFailureStore) RollbackWorkspacePtyControlOperation(_ context.Context, params db.RollbackWorkspacePtyControlOperationParams) (db.WorkspacePtySession, error) {
	s.ptyControlRollbacks = append(s.ptyControlRollbacks, params)
	return db.WorkspacePtySession{}, nil
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
	if len(store.ptyFailed) != 0 || len(store.ptyControlRollbacks) != 0 {
		t.Fatalf("pty cleanup = failed %d rollbacks %d, want none", len(store.ptyFailed), len(store.ptyControlRollbacks))
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

func TestFailWorkspacePrimitiveForOperationRollsBackPtyControlOperations(t *testing.T) {
	store := &primitiveFailureStore{}

	for _, tc := range []struct {
		kind    db.WorkspaceMaterializationOperationKind
		request []byte
	}{
		{kind: workspaceOperationKindResizePty, request: []byte(`{"pty_id":"pty-1","cols":120,"rows":40}`)},
		{kind: workspaceOperationKindClosePty, request: []byte(`{"pty_id":"pty-1"}`)},
	} {
		operation := testWorkspaceMaterializationOperation(tc.kind, workspaceOperationResourcePty)
		operation.Request = tc.request
		if err := failWorkspacePrimitiveForOperation(context.Background(), store, operation, []byte(`{"code":"control_failed"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.ptyControlRollbacks) != 2 {
		t.Fatalf("pty control rollbacks = %d, want 2", len(store.ptyControlRollbacks))
	}
	for _, got := range store.ptyControlRollbacks {
		if got.ID != testControlUUID(6) || got.MaterializationID != testControlUUID(5) {
			t.Fatalf("rollback scope = %+v", got)
		}
	}
	if store.ptyControlRollbacks[0].OperationKind != workspaceOperationKindResizePty ||
		!store.ptyControlRollbacks[0].Cols.Valid || store.ptyControlRollbacks[0].Cols.Int32 != 120 ||
		!store.ptyControlRollbacks[0].Rows.Valid || store.ptyControlRollbacks[0].Rows.Int32 != 40 {
		t.Fatalf("resize rollback target = %+v", store.ptyControlRollbacks[0])
	}
	if store.ptyControlRollbacks[1].OperationKind != workspaceOperationKindClosePty ||
		store.ptyControlRollbacks[1].Cols.Valid || store.ptyControlRollbacks[1].Rows.Valid {
		t.Fatalf("close rollback target = %+v", store.ptyControlRollbacks[1])
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

func testWorkspaceMaterializationOperation(kind db.WorkspaceMaterializationOperationKind, resourceKind db.WorkspaceResourceKind) db.WorkspaceMaterializationOperation {
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
