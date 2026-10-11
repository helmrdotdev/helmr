package idempotency_test

import (
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	retry "github.com/helmrdotdev/helmr/internal/idempotency"
	"testing"
	"uuid"
)

func TestPlatformCommandCreationAndCancellationRetainReceiptsUntilPhysicalStop(t *testing.T) {
	f := agenttest.New(t)
	command := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(environment_id,id,computer_id,computer_lease_epoch,status,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,1,'running',ARRAY['true'],'{}',''::bytea,1000,'api_key','fixture')`, f.Environment, command, f.Computer)
	create, _ := retry.NewComputerCommandRequest(f.Environment, f.Computer, "create", retry.ComputerCommandFingerprint{Command: []string{"true"}, TimeoutMS: 1000})
	cancel, _ := retry.NewCommandCancelRequest(f.Environment, command)
	a := accept(t, f, create, retry.Target{CommandID: command})
	b := accept(t, f, cancel, retry.Target{CommandID: command})
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE platform_retry_keys SET accepted_at=accepted_at-interval '31 days',receipt_expires_at=receipt_expires_at-interval '31 days' WHERE environment_id=$1`, f.Environment)
	for _, request := range []retry.Request{create, cancel} {
		if err := acquireError(t, f, request); err != nil {
			t.Fatalf("live Command lost receipt: %v", err)
		}
	}
	q := db.New(f.Pool)
	if n, err := q.PruneExpiredPlatformRetryReceipts(t.Context(), 100); err != nil || n != 0 {
		t.Fatalf("live command pruned %d: %v", n, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='cancelled',terminal_at=clock_timestamp(),terminal_reason_code='computer_command_cancelled' WHERE environment_id=$1 AND id=$2`, f.Environment, command)
	if n, err := q.PruneExpiredPlatformRetryReceipts(t.Context(), 100); err != nil || n != 0 {
		t.Fatalf("unreconciled command pruned %d: %v", n, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET process_reconciled_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.Environment, command)
	locked, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), locked, `SELECT id FROM platform_retry_keys WHERE environment_id=$1 AND id=$2 FOR UPDATE`, f.Environment, a.ID)
	if n, err := q.PruneExpiredPlatformRetryReceipts(t.Context(), 100); err != nil || n != 1 {
		t.Fatalf("skip locked receipts %d: %v", n, err)
	}
	if err = locked.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := q.PruneExpiredPlatformRetryReceipts(t.Context(), 100); err != nil || n != 1 {
		t.Fatalf("released receipt %d: %v", n, err)
	}
	var retained bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*)=2 AND count(receipt)=0 FROM platform_retry_keys WHERE environment_id=$1 AND id=ANY($2::uuid[])`, f.Environment, []uuid.UUID{uuid.UUID(a.ID.Bytes), uuid.UUID(b.ID.Bytes)}).Scan(&retained); err != nil || !retained {
		t.Fatalf("identity retention: %v", err)
	}
}
