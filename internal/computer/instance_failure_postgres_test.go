package computer

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

// preparingInstance is runningInstance returned to preparation: the Instance
// is allocated and holds the Computer's preparation after attempts, and the
// lease's Run is queued again.
func preparingInstance(t *testing.T, attempts int) (runtest.Fixture, runtest.RunLease, db.ComputerInstance) {
	t.Helper()
	f, work, i := runningInstance(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_desired_version=0,ready_at=NULL WHERE id=$1`, i.ID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET preparation_attempt_count=$2,preparation_instance_id=$1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, i.ID, attempts)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET current_run_lease_id=NULL,status='queued' WHERE id=$1`, work.RunID)
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: i.ID})
	if err != nil {
		t.Fatal(err)
	}
	return f, work, i
}

// A runtime failure report is durable but settles no logical work: the
// failed Instance keeps its reservation, the Computer its preparation budget
// and the Run stays queued, and the report cannot be applied twice.
func TestInstanceFailureIsDurableWithoutLogicalSettlement(t *testing.T) {
	f, work, i := preparingInstance(t, 8)
	failure := Failure{Observation: observationOf(i), Kind: FailureRuntime, Reason: "runtime_reconcile_failed"}
	failed, err := RecordInstanceFailure(t.Context(), f.Pool, failure)
	if err != nil {
		t.Fatal(err)
	}
	if failed.ObservedState != "failed" || failed.DesiredState != "closed" || failed.ReclaimedAt.Valid || failed.ReservedCPUMillis != i.ReservedCPUMillis {
		t.Fatalf("failure released capacity: %+v", failed)
	}
	var count int
	var settled bool
	var status string
	if err = f.Pool.QueryRow(t.Context(), `SELECT preparation_attempt_count,preparation_failure IS NOT NULL,(SELECT status FROM runs WHERE id=$2) FROM computers WHERE id=$1`, i.ComputerID, work.RunID).Scan(&count, &settled, &status); err != nil {
		t.Fatal(err)
	}
	if count != 8 || settled || status != "queued" {
		t.Fatalf("physical reporter settled logical work: %d/%v/%s", count, settled, status)
	}
	if _, err = RecordInstanceFailure(t.Context(), f.Pool, failure); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("replayed physical failure=%v", err)
	}
}

// A source-unavailable failure report records the Computer's recovery
// failure and keeps its source disk, the unreclaimed Instance, the Computer's
// preparation budget and its pending members for reconciliation.
func TestSourceFailureKeepsSourceAndPendingMembers(t *testing.T) {
	f, work, i := preparingInstance(t, 1)
	commandID, claimID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claimID, f.EnvironmentID, dbtest.Hash(claimID.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id)
 SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text FROM run_leases WHERE id=$1`, work.LeaseID, commandID, claimID)
	failure := Failure{Observation: observationOf(i), Kind: FailureSourceUnavailable, Reason: "computer_source_unavailable", Error: []byte(`{"code":"source_missing"}`)}
	if _, err := RecordInstanceFailure(t.Context(), f.Pool, failure); err != nil {
		t.Fatal(err)
	}
	var untouched bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.preparation_attempt_count=1 AND c.preparation_failure IS NULL AND c.recovery_failure IS NOT NULL AND r.status='queued' AND p.status='pending' AND c.head_disk_version_id=i.source_disk_version_id AND i.reclaimed_at IS NULL FROM computers c JOIN computer_instances i ON i.computer_id=c.id JOIN runs r ON r.id=$2 JOIN computer_commands p ON p.id=$3 WHERE i.id=$1`, i.ID, work.RunID, commandID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("reporter changed logical ownership or lost source: %v %v", untouched, err)
	}
}
