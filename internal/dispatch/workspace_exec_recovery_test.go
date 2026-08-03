package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type workspaceExecRecoveryDiscovery struct {
	recoverable []db.ListRecoverableWorkspaceExecCandidatesRow
	pending     []db.ListPendingWorkspaceExecCandidatesRow
}

func (d workspaceExecRecoveryDiscovery) ListRecoverableWorkspaceExecCandidates(
	context.Context,
	int32,
) ([]db.ListRecoverableWorkspaceExecCandidatesRow, error) {
	return d.recoverable, nil
}

func (d workspaceExecRecoveryDiscovery) ListPendingWorkspaceExecCandidates(
	context.Context,
	int32,
) ([]db.ListPendingWorkspaceExecCandidatesRow, error) {
	return d.pending, nil
}

type workspaceExecRecoveryAuthority struct {
	calls []string
}

func (a *workspaceExecRecoveryAuthority) RecoverWorkspaceExec(
	_ context.Context,
	_ RecoverableWorkspaceExecCandidate,
) error {
	a.calls = append(a.calls, "recover")
	return nil
}

func (a *workspaceExecRecoveryAuthority) PlaceWorkspaceExec(
	context.Context,
	ReadyWorkspaceExecCandidate,
) (WorkspaceExecPlacement, error) {
	a.calls = append(a.calls, "place")
	return WorkspaceExecPlacement{}, nil
}

func (a *workspaceExecRecoveryAuthority) FailPendingWorkspaceExec(
	context.Context,
	ReadyWorkspaceExecCandidate,
	string,
) error {
	a.calls = append(a.calls, "fail")
	return nil
}

func TestReconcileWorkspaceExecsRecoversLostAuthorityBeforePlacement(t *testing.T) {
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	processID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workspaceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	authority := &workspaceExecRecoveryAuthority{}
	reconciler := PlacementReconciler{
		workspaceExecDiscovery: workspaceExecRecoveryDiscovery{
			recoverable: []db.ListRecoverableWorkspaceExecCandidatesRow{{
				OrgID: orgID, ID: processID, WorkspaceID: workspaceID, StateVersion: 3,
			}},
			pending: []db.ListPendingWorkspaceExecCandidatesRow{{
				OrgID: orgID, ID: processID, StateVersion: 4,
				CreatedAt: pgvalue.TimestamptzUTCZeroInvalid(
					time.Now().Add(-time.Minute),
				),
			}},
		},
		workspaceExecAuthority: authority,
		workspaceExecPolicy:    placementLoopPolicy{limit: 8},
	}

	if err := reconciler.ReconcileWorkspaceExecs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(authority.calls) != 2 ||
		authority.calls[0] != "recover" ||
		authority.calls[1] != "place" {
		t.Fatalf("calls = %v, want [recover place]", authority.calls)
	}
}

func TestReconcileWorkspaceExecsFailsExpiredPendingCandidate(t *testing.T) {
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	processID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	authority := &workspaceExecRecoveryAuthority{}
	reconciler := PlacementReconciler{
		workspaceExecDiscovery: workspaceExecRecoveryDiscovery{
			pending: []db.ListPendingWorkspaceExecCandidatesRow{{
				OrgID: orgID, ID: processID, StateVersion: 1,
				CreatedAt: pgvalue.TimestamptzUTCZeroInvalid(
					time.Now().Add(-11 * time.Minute),
				),
			}},
		},
		workspaceExecAuthority: authority,
		workspaceExecPolicy:    placementLoopPolicy{limit: 8},
	}

	if err := reconciler.ReconcileWorkspaceExecs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(authority.calls) != 1 || authority.calls[0] != "fail" {
		t.Fatalf("calls = %v, want [fail]", authority.calls)
	}
}

func TestClassifyWorkspaceExecRecovery(t *testing.T) {
	stagedVersionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	tests := []struct {
		name      string
		process   db.WorkspaceProcessState
		kind      pgtype.Text
		reason    pgtype.Text
		errorJSON []byte
		staged    pgtype.UUID
		secrets   bool
		want      workspaceExecRecoveryKind
	}{
		{
			name: "staged capture", process: db.WorkspaceProcessStateExitRequested,
			kind: pgvalue.Text("capture"), reason: pgvalue.Text("workspace_exec_completed"),
			staged: stagedVersionID, secrets: true, want: workspaceExecRecoveryCapture,
		},
		{
			name: "revoked staged capture", process: db.WorkspaceProcessStateExitRequested,
			kind: pgvalue.Text("capture"), reason: pgvalue.Text("workspace_exec_completed"),
			staged: stagedVersionID, want: workspaceExecRecoveryRevoked,
		},
		{
			name: "discard rollback", process: db.WorkspaceProcessStateExitRequested,
			kind: pgvalue.Text("discard"), reason: pgvalue.Text("workspace_exec_timed_out"),
			errorJSON: []byte(`{"code":"workspace_exec_timed_out"}`),
			want:      workspaceExecRecoveryDiscard,
		},
		{
			name: "capture before staging", process: db.WorkspaceProcessStateExitRequested,
			kind: pgvalue.Text("capture"), reason: pgvalue.Text("workspace_exec_completed"),
			want: workspaceExecRecoveryUncertain,
		},
		{
			name: "discard with staged version", process: db.WorkspaceProcessStateExitRequested,
			kind: pgvalue.Text("discard"), reason: pgvalue.Text("workspace_exec_timed_out"),
			staged: stagedVersionID, want: workspaceExecRecoveryUncertain,
		},
		{
			name: "running without finalization", process: db.WorkspaceProcessStateRunning,
			want: workspaceExecRecoveryUncertain,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyWorkspaceExecRecovery(
				db.LockWorkspaceExecRecoveryAuthorityRow{
					WorkspaceProcess: db.WorkspaceProcess{State: test.process},
					WorkspaceMount: db.WorkspaceMount{
						FinalizationKind:       test.kind,
						FinalizationReasonCode: test.reason,
						FinalizationError:      test.errorJSON,
						StagedVersionID:        test.staged,
					},
				},
				test.secrets,
			)
			if got != test.want {
				t.Fatalf("classification = %v, want %v", got, test.want)
			}
		})
	}
}
