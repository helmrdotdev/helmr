package command

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

// Recovery records a lost result once the Instance's writer is lost and
// reconciles the process scope only after the Instance was reclaimed.
func TestRecoverSettlesLostInstanceAndReconcilesOnReclaim(t *testing.T) {
	f := runtest.New(t)
	lease := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	bound := commandtest.Bound(t, f, lease.LeaseID, "running")
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM computer_commands WHERE id=$1`, bound.ID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	candidate := func() RecoveryCandidate {
		t.Helper()
		c := RecoveryCandidate{OrgID: f.OrgID, CommandID: bound.ID, ComputerID: computerID}
		if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM computer_commands WHERE id=$1`, bound.ID).Scan(&c.ExpectedRevision); err != nil {
			t.Fatal(err)
		}
		return c
	}
	for name, mutate := range map[string]func(*RecoveryCandidate){
		"revision":     func(c *RecoveryCandidate) { c.ExpectedRevision++ },
		"computer":     func(c *RecoveryCandidate) { c.ComputerID = uuid.NewV7() },
		"organization": func(c *RecoveryCandidate) { c.OrgID = uuid.NewV7() },
	} {
		stale := candidate()
		mutate(&stale)
		if err := Recover(t.Context(), f.Pool, stale); !errors.Is(err, ErrChanged) {
			t.Fatalf("stale %s: %v", name, err)
		}
	}
	if err := Recover(t.Context(), f.Pool, candidate()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_commands WHERE id=$1`, bound.ID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("live Command recovered: %s %v", status, err)
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, bound.InstanceID)
	if err := Recover(t.Context(), f.Pool, candidate()); err != nil {
		t.Fatal(err)
	}
	var detail string
	var reconciled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status,error::text,process_reconciled_at IS NOT NULL FROM computer_commands WHERE id=$1`, bound.ID).Scan(&status, &detail, &reconciled); err != nil {
		t.Fatal(err)
	}
	if status != "lost" || detail != `{"code": "computer_instance_lost", "retryable": false}` || reconciled {
		t.Fatalf("lost Command status=%s error=%s reconciled=%v", status, detail, reconciled)
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances
 SET desired_state='closed',desired_version=desired_version+1,observed_state='closed',observed_version=observed_version+1,observed_desired_version=desired_version+1,
 terminal_at=now(),terminal_reason_code='execution_lost',reclaimed_at=now(),mount_state='unmounted',unmounted_at=now(),admission_state='closed',
 reclaim_evidence='{"method":"host_reconciled"}' WHERE id=$1`, bound.InstanceID)
	for range 2 {
		if err := Recover(t.Context(), f.Pool, candidate()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NOT NULL FROM computer_commands WHERE id=$1`, bound.ID).Scan(&reconciled); err != nil || !reconciled {
		t.Fatalf("reclaimed Command reconciled=%v err=%v", reconciled, err)
	}
}
