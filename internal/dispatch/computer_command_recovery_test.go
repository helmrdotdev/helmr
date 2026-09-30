package dispatch

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

type computerCommandRecoveryDiscovery struct {
	recoverable []db.ListRecoverableComputerCommandCandidatesRow
	pending     []db.ListPendingComputerCommandCandidatesRow
}

func (d computerCommandRecoveryDiscovery) ListRecoverableComputerCommandCandidates(
	context.Context,
	int32,
) ([]db.ListRecoverableComputerCommandCandidatesRow, error) {
	return d.recoverable, nil
}

func (d computerCommandRecoveryDiscovery) ListPendingComputerCommandCandidates(
	context.Context,
	int32,
) ([]db.ListPendingComputerCommandCandidatesRow, error) {
	return d.pending, nil
}

type computerCommandRecoveryAuthority struct {
	calls []string
}

func (a *computerCommandRecoveryAuthority) RecoverComputerCommand(
	_ context.Context,
	_ RecoverableComputerCommandCandidate,
) error {
	a.calls = append(a.calls, "recover")
	return nil
}

func (a *computerCommandRecoveryAuthority) AssignCommand(
	context.Context,
	CommandCandidate,
) (CommandAssignment, error) {
	a.calls = append(a.calls, "assign")
	return CommandAssignment{}, nil
}

func (a *computerCommandRecoveryAuthority) FailPendingComputerCommand(
	context.Context,
	CommandCandidate,
	string,
) error {
	a.calls = append(a.calls, "fail")
	return nil
}

func TestReconcileComputerCommandsRecoversLostAuthorityBeforeAssignment(t *testing.T) {
	orgID := pgvalue.UUID(uuid.NewV7())
	commandID := pgvalue.UUID(uuid.NewV7())
	computerID := pgvalue.UUID(uuid.NewV7())
	authority := &computerCommandRecoveryAuthority{}
	reconciler := Reconciler{
		computerCommandDiscovery: computerCommandRecoveryDiscovery{
			recoverable: []db.ListRecoverableComputerCommandCandidatesRow{{
				OrgID: orgID, ID: commandID, ComputerID: computerID, Revision: 3,
			}},
			pending: []db.ListPendingComputerCommandCandidatesRow{{
				OrgID: orgID, ID: commandID, Revision: 4,
				CreatedAt: pgvalue.TimestamptzUTCZeroInvalid(
					time.Now().Add(-time.Minute),
				),
			}},
		},
		computerCommandAuthority: authority,
		computerCommandPolicy:    loopPolicy{limit: 8},
	}

	if err := reconciler.ReconcileComputerCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(authority.calls) != 2 ||
		authority.calls[0] != "recover" ||
		authority.calls[1] != "assign" {
		t.Fatalf("calls = %v, want [recover assign]", authority.calls)
	}
}

func TestReconcileComputerCommandsFailsExpiredPendingCandidate(t *testing.T) {
	orgID := pgvalue.UUID(uuid.NewV7())
	commandID := pgvalue.UUID(uuid.NewV7())
	authority := &computerCommandRecoveryAuthority{}
	reconciler := Reconciler{
		computerCommandDiscovery: computerCommandRecoveryDiscovery{
			pending: []db.ListPendingComputerCommandCandidatesRow{{
				OrgID: orgID, ID: commandID, Revision: 1,
				CreatedAt: pgvalue.TimestamptzUTCZeroInvalid(
					time.Now().Add(-11 * time.Minute),
				),
			}},
		},
		computerCommandAuthority: authority,
		computerCommandPolicy:    loopPolicy{limit: 8},
	}

	if err := reconciler.ReconcileComputerCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(authority.calls) != 1 || authority.calls[0] != "fail" {
		t.Fatalf("calls = %v, want [fail]", authority.calls)
	}
}
