package dispatch

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCommandReceiptRetainedUntilPhysicalReconciliation(t *testing.T) {
	f, commandID, computerID, recoverCommand := commandRetentionFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE idempotency_claims
 SET receipt_expires_at=now()-interval '1 day'
 WHERE id=(SELECT claim_id FROM computer_commands WHERE id=$1)`, commandID)
	queries := db.New(f.Pool)
	prune := func(want int64) {
		t.Helper()
		if count, err := queries.PruneExpiredIdempotencyReceipts(t.Context(), 100); err != nil || count != want {
			t.Fatalf("collect Command receipt = %d, %v; want %d", count, err, want)
		}
	}
	prune(0)
	members, err := queries.ListComputerMembers(t.Context(), db.ListComputerMembersParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: computerID, RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, member := range members {
		if member.Kind == "command" && member.ID == commandID {
			found = true
		}
	}
	if !found {
		t.Fatal("unreconciled Command missing from Computer members")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands
 SET status='failed',failure_reason='guest_failure',terminal_at=now(),terminal_reason_code='execution_lost' WHERE id=$1`, commandID)
	prune(0)
	members, err = queries.ListComputerMembers(t.Context(), db.ListComputerMembersParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: computerID, RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, member := range members {
		if member.Kind == "command" && member.ID == commandID {
			found = true
		}
	}
	if !found {
		t.Fatal("terminal unreconciled Command missing from Computer members")
	}
	for _, member := range members {
		if member.ID == commandID && (member.State != "unreconciled" || member.RunID.Valid) {
			t.Fatalf("unreconciled Command projection=%+v", member)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances
 SET desired_state='closed', desired_version=desired_version+1, observed_state='closed',observed_version=observed_version+1,observed_desired_version=desired_version+1, terminal_at=now(),
 terminal_reason_code='execution_lost',reclaimed_at=now(),mount_state='unmounted',unmounted_at=now(),admission_state='closed',
 reclaim_evidence='{"method":"host_reconciled"}'
 WHERE id=(SELECT computer_instance_id FROM computer_commands WHERE id=$1)`, commandID)
	recoverCommand(true)
	prune(1)
	members, err = queries.ListComputerMembers(t.Context(), db.ListComputerMembersParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: computerID, RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.Kind == "command" && member.ID == commandID {
			t.Fatal("reconciled Command retained as a Computer member")
		}
	}
	prune(0)
}

func TestCommandPayloadCollectionRequiresExpiredReconciledScope(t *testing.T) {
	f, commandID, _, recoverCommand := commandRetentionFixture(t)
	queries := db.New(f.Pool)
	prune := func(want int64) {
		t.Helper()
		if count, err := queries.PruneExpiredComputerCommandResults(t.Context(), 100); err != nil || count != want {
			t.Fatalf("prune Command payload = %d, %v; want %d", count, err, want)
		}
	}
	prune(0)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands
 SET status='failed',failure_reason='guest_failure',terminal_at=now(),terminal_reason_code='execution_lost',
     result_expires_at=now()-interval '1 day',error='{"code":"execution_lost"}'
 WHERE id=$1`, commandID)
	recoverCommand(false)
	prune(0)
	if _, err := f.Pool.Exec(t.Context(), `UPDATE computer_commands
 SET argv=NULL,cwd=NULL,env=NULL,stdin=NULL,error=NULL,result_pruned_at=now() WHERE id=$1`, commandID); err == nil {
		t.Fatal("schema accepted pruning an unreconciled execution")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances
 SET desired_state='closed',desired_version=desired_version+1,observed_state='closed',observed_version=observed_version+1,observed_desired_version=desired_version+1,
 terminal_at=now(),terminal_reason_code='execution_lost',reclaimed_at=now(),mount_state='unmounted',unmounted_at=now(),admission_state='closed',
 reclaim_evidence='{"method":"host_reconciled"}'
 WHERE id=(SELECT computer_instance_id FROM computer_commands WHERE id=$1)`, commandID)
	recoverCommand(true)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET result_expires_at=now()+interval '1 day' WHERE id=$1`, commandID)
	prune(0)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET result_expires_at=now()-interval '1 day' WHERE id=$1`, commandID)
	locked, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), locked, `SELECT id FROM computer_commands WHERE id=$1 FOR UPDATE`, commandID)
	prune(0)
	if err := locked.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	prune(1)
	prune(0)
	var payloadGone, identityKept bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT
 argv IS NULL AND cwd IS NULL AND env IS NULL AND stdin IS NULL AND error IS NULL
 AND result_pruned_at IS NOT NULL,
 status='failed' AND terminal_reason_code='execution_lost'
 AND process_reconciled_at IS NOT NULL AND claim_id IS NOT NULL AND computer_instance_id IS NOT NULL
 FROM computer_commands WHERE id=$1`, commandID).Scan(&payloadGone, &identityKept); err != nil {
		t.Fatal(err)
	}
	if !payloadGone || !identityKept {
		t.Fatalf("pruned payload=%v retained identity=%v", payloadGone, identityKept)
	}
}

func commandRetentionFixture(t *testing.T) (runtest.Fixture, pgtype.UUID, pgtype.UUID, func(bool)) {
	t.Helper()
	f, work, a := commandPlacementFixture(t)
	candidate := pendingSharedCommand(t, f, work)
	placed, err := a.PlaceComputerCommand(t.Context(), candidate)
	if err != nil || !placed.ProcessBound {
		t.Fatalf("place Command: %+v %v", placed, err)
	}
	var computerID pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM computer_commands WHERE id=$1`, candidate.CommandID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE idempotency_claims SET status='completed',completed_at=now(),receipt=jsonb_build_object('command_id',$1::text),receipt_expires_at=now()+interval '1 day' WHERE id=(SELECT claim_id FROM computer_commands WHERE id=$1::uuid)`, candidate.CommandID)
	recoverCommand := func(want bool) {
		t.Helper()
		var revision int64
		if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM computer_commands WHERE id=$1`, candidate.CommandID).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if err := a.RecoverComputerCommand(t.Context(), RecoverableComputerCommandCandidate{OrgID: candidate.OrgID, CommandID: candidate.CommandID, ComputerID: computerID, ExpectedRevision: revision}); err != nil {
			t.Fatal(err)
		}
		var reconciled bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NOT NULL FROM computer_commands WHERE id=$1`, candidate.CommandID).Scan(&reconciled); err != nil || reconciled != want {
			t.Fatalf("physical reconciliation=%v want=%v err=%v", reconciled, want, err)
		}
	}
	return f, candidate.CommandID, computerID, recoverCommand
}
