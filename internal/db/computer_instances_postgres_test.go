package db

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func instanceFixture(t *testing.T, allocated bool) (runtest.Fixture, ComputerInstance) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	var id string
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id::text FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// No live member remains while testing physical lifecycle transitions.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='fixture',process_reconciled_at=now() WHERE id=$1`, work.LeaseID)
	if allocated {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_version=0,observed_desired_version=0,ready_at=NULL,mount_state='pending',mounted_at=NULL,preparation_expires_at=now()+interval '5 minutes' WHERE id=$1`, id)
	}
	i, err := New(f.Pool).GetComputerInstance(t.Context(), GetComputerInstanceParams{ID: pgvalue.UUID(uuid.MustParse(id)), EnvironmentID: pgvalue.UUID(f.EnvironmentID)})
	if err != nil {
		t.Fatal(err)
	}
	return f, i
}

func readyInstanceParams(i ComputerInstance) MarkComputerInstanceReadyParams {
	return MarkComputerInstanceReadyParams{ID: i.ID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, WriterGeneration: i.WriterGeneration, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, VMVCPUCount: i.VMVCPUCount, CPUConfigDigest: i.CPUConfigDigest}
}
func reclaimInstanceParams(i ComputerInstance) ReclaimComputerInstanceParams {
	return ReclaimComputerInstanceParams{ID: i.ID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, WriterGeneration: i.WriterGeneration, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, ObservedState: "closed", MountState: "unmounted", Reason: pgvalue.Text("closed"), Evidence: []byte(`{"method":"session_closed"}`)}
}
func TestComputerInstanceAllocatedReadyClosedPath(t *testing.T) {
	f, i := instanceFixture(t, true)
	q := New(f.Pool)
	ready, err := q.MarkComputerInstanceReady(t.Context(), readyInstanceParams(i))
	if err != nil {
		t.Fatal(err)
	}
	if ready.ObservedState != "ready" || ready.MountState != "mounted" || ready.ObservedVersion != i.ObservedVersion+1 || !ready.ReadyAt.Valid || ready.ReclaimedAt.Valid {
		t.Fatalf("ready=%+v", ready)
	}
	closed, err := q.RequestComputerInstanceClose(t.Context(), RequestComputerInstanceCloseParams{ID: i.ID, WriterGeneration: i.WriterGeneration, Reason: "idle", FinalizationAction: pgvalue.Text("discard")})
	if err != nil {
		t.Fatal(err)
	}
	if closed.DesiredState != "closed" || closed.AdmissionState != "closed" || closed.MountState != "unmounting" || closed.ReclaimedAt.Valid {
		t.Fatalf("close intent=%+v", closed)
	}
	reclaimed, err := q.ReclaimComputerInstance(t.Context(), reclaimInstanceParams(closed))
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.ObservedState != "closed" || reclaimed.MountState != "unmounted" || !reclaimed.ReclaimedAt.Valid || !reclaimed.UnmountedAt.Valid || !reclaimed.TerminalAt.Valid {
		t.Fatalf("reclaimed=%+v", reclaimed)
	}
	if _, err = q.MarkComputerInstanceReady(t.Context(), readyInstanceParams(reclaimed)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reclaimed readmission=%v", err)
	}
}

func TestComputerInstanceFailureFromAllocatedAndReady(t *testing.T) {
	for _, allocated := range []bool{true, false} {
		t.Run(map[bool]string{true: "allocated", false: "ready"}[allocated], func(t *testing.T) {
			f, i := instanceFixture(t, allocated)
			q := New(f.Pool)
			failed, err := q.MarkComputerInstanceFailed(t.Context(), MarkComputerInstanceFailedParams{ID: i.ID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, ReasonCode: pgvalue.Text("instance_failed"), Error: []byte(`{"code":"instance_failed"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if failed.ObservedState != "failed" || failed.DesiredState != "closed" || failed.DesiredVersion != i.DesiredVersion+1 || !failed.TerminalAt.Valid || failed.ReclaimedAt.Valid || failed.ReadyAt.Valid == allocated {
				t.Fatalf("failed=%+v", failed)
			}
			params := reclaimInstanceParams(failed)
			params.RequireFailure = true
			params.ObservedState = "failed"
			reclaimed, err := q.ReclaimComputerInstance(t.Context(), params)
			if err != nil {
				t.Fatal(err)
			}
			if !reclaimed.ReclaimedAt.Valid || reclaimed.ObservedState != "failed" {
				t.Fatalf("reclaim=%+v", reclaimed)
			}
		})
	}
}

func TestComputerInstanceStaleFencesAreRejected(t *testing.T) {
	f, i := instanceFixture(t, true)
	q := New(f.Pool)
	params := readyInstanceParams(i)
	for _, tc := range []struct {
		name   string
		change func(*MarkComputerInstanceReadyParams)
	}{
		{"worker epoch", func(p *MarkComputerInstanceReadyParams) { p.WorkerEpoch++ }},
		{"writer generation", func(p *MarkComputerInstanceReadyParams) { p.WriterGeneration++ }},
		{"observed version", func(p *MarkComputerInstanceReadyParams) { p.ExpectedObservedVersion++ }},
		{"desired version", func(p *MarkComputerInstanceReadyParams) { p.DesiredVersion++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := params
			tc.change(&p)
			if _, err := q.MarkComputerInstanceReady(t.Context(), p); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale ready=%v", err)
			}
		})
	}
	if _, err := q.MarkComputerInstanceReady(t.Context(), params); err != nil {
		t.Fatal(err)
	}
}

func TestComputerInstanceReclaimRequiresCloseAndCurrentFence(t *testing.T) {
	f, i := instanceFixture(t, false)
	q := New(f.Pool)
	if _, err := q.ReclaimComputerInstance(t.Context(), reclaimInstanceParams(i)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reclaim without close=%v", err)
	}
	closed, err := q.RequestComputerInstanceClose(t.Context(), RequestComputerInstanceCloseParams{ID: i.ID, WriterGeneration: i.WriterGeneration, Reason: "idle", FinalizationAction: pgvalue.Text("discard")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*ReclaimComputerInstanceParams)
	}{
		{"writer generation", func(p *ReclaimComputerInstanceParams) { p.WriterGeneration++ }},
		{"epoch", func(p *ReclaimComputerInstanceParams) { p.WorkerEpoch++ }},
		{"desired version", func(p *ReclaimComputerInstanceParams) { p.DesiredVersion++ }},
		{"observation", func(p *ReclaimComputerInstanceParams) { p.ExpectedObservedVersion++ }},
		{"failure required", func(p *ReclaimComputerInstanceParams) { p.RequireFailure = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := reclaimInstanceParams(closed)
			tc.change(&p)
			if _, err := q.ReclaimComputerInstance(t.Context(), p); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalid reclaim=%v", err)
			}
		})
	}
	if _, err := q.ReclaimComputerInstance(t.Context(), reclaimInstanceParams(closed)); err != nil {
		t.Fatal(err)
	}
}

func TestComputerInstanceRowShapes(t *testing.T) {
	f, i := instanceFixture(t, false)
	for _, tc := range []struct{ name, sql string }{
		{"ready without time", `UPDATE computer_instances SET ready_at=NULL WHERE id=$1`},
		{"allocated with terminal", `UPDATE computer_instances SET observed_state='allocated',ready_at=NULL,terminal_at=now(),terminal_reason_code='invalid' WHERE id=$1`},
		{"reclaim without proof", `UPDATE computer_instances SET reclaimed_at=now() WHERE id=$1`},
		{"closed without reclaim", `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',terminal_at=now(),terminal_reason_code='closed' WHERE id=$1`},
		{"closed with terminal error", `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',terminal_at=now(),terminal_reason_code='closed',mount_state='unmounted',unmounted_at=now(),admission_state='closed',reclaimed_at=now(),reclaim_evidence='{"method":"test"}',terminal_error='{}' WHERE id=$1`},
		{"failed without reason", `UPDATE computer_instances SET observed_state='failed',terminal_at=now(),terminal_reason_code=NULL WHERE id=$1`},
		{"proof without reclaim time", `UPDATE computer_instances SET reclaim_evidence='{"method":"test"}' WHERE id=$1`},
		{"reclaim before terminal", `UPDATE computer_instances SET observed_state='failed',terminal_at=now(),terminal_reason_code='failed',mount_state='unmounted',unmounted_at=now(),admission_state='closed',reclaimed_at=now()-interval '1 second',reclaim_evidence='{"method":"test"}' WHERE id=$1`},
		{"unmounted without time", `UPDATE computer_instances SET mount_state='unmounted',unmounted_at=NULL WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Pool.Exec(t.Context(), tc.sql, i.ID)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
				t.Fatalf("expected check constraint, got %v", err)
			}
			expected := map[string]string{
				"ready without time":         "computer_instances_observation_shape_check",
				"allocated with terminal":    "computer_instances_observation_shape_check",
				"closed without reclaim":     "computer_instances_observation_shape_check",
				"closed with terminal error": "computer_instances_observation_shape_check",
				"failed without reason":      "computer_instances_observation_shape_check",
				"proof without reclaim time": "computer_instances_reclaim_evidence_pair_check",
				"reclaim before terminal":    "computer_instances_reclaim_time_check",
			}[tc.name]
			if expected != "" && pgerr.ConstraintName != expected {
				t.Fatalf("constraint=%s want=%s", pgerr.ConstraintName, expected)
			}
		})
	}
}

func TestComputerInstanceFailureRejectsStaleObservation(t *testing.T) {
	f, i := instanceFixture(t, false)
	p := MarkComputerInstanceFailedParams{ID: i.ID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion + 1, ReasonCode: pgvalue.Text("failed"), Error: []byte(`{"code":"failed"}`)}
	if _, err := New(f.Pool).MarkComputerInstanceFailed(t.Context(), p); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale failure=%v", err)
	}
	current, err := New(f.Pool).GetComputerInstance(t.Context(), GetComputerInstanceParams{ID: i.ID, EnvironmentID: i.EnvironmentID})
	if err != nil {
		t.Fatal(err)
	}
	if current.ObservedVersion != i.ObservedVersion || current.ObservedState != i.ObservedState || current.DesiredVersion != i.DesiredVersion || current.TerminalAt.Valid {
		t.Fatalf("stale failure mutated instance=%+v", current)
	}
}
