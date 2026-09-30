package computer

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// residentComputer is a Computer with a ready Instance, from a running Run
// lease, and its Instance.
func (f fixture) residentComputer(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	work := f.AddRunLease(t, "running", time.Now())
	var computerID, instanceID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id,computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&computerID, &instanceID); err != nil {
		t.Fatal(err)
	}
	return computerID, instanceID
}

func (f fixture) beginFence(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// locked reports whether another transaction holds a row lock on the row of
// table with the id.
func (f fixture) locked(t *testing.T, table string, id uuid.UUID) bool {
	t.Helper()
	probe, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Rollback(context.Background()) }()
	_, err = probe.Exec(t.Context(), `SELECT 1 FROM `+table+` WHERE id=$1 FOR UPDATE NOWAIT`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	return false
}

func TestRunResidenceOutsideTheRunScopeLocksNoInstance(t *testing.T) {
	f := newFixture(t)
	computerID, instanceID := f.residentComputer(t)
	ref := RunResidenceRef{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RegionID: runtest.Region, ComputerID: computerID}

	outside := ref
	outside.RegionID = "other-region"
	tx := f.beginFence(t)
	if _, err := LockRunResidence(t.Context(), tx, outside); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outside the Run scope = %v, want ErrNotFound", err)
	}
	if f.locked(t, "computers", computerID) || f.locked(t, "computer_instances", instanceID) {
		t.Fatal("a Run residence outside the Run scope locked the Computer or its Instance")
	}

	tx = f.beginFence(t)
	residence, err := LockRunResidence(t.Context(), tx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if residence.Computer().ID != pgvalue.UUID(computerID) {
		t.Fatalf("residence Computer = %v, want %s", residence.Computer().ID, computerID)
	}
	if !f.locked(t, "computers", computerID) || !f.locked(t, "computer_instances", instanceID) {
		t.Fatal("a Run residence did not lock the Computer and its Instance")
	}
}

func TestResidenceLocksTheComputerAndAnyUnreclaimedInstance(t *testing.T) {
	f := newFixture(t)
	if err := LockResidence(t.Context(), f.beginFence(t), f.EnvironmentID, uuid.NewV7()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Computer residence = %v, want ErrNotFound", err)
	}

	computerID, instanceID := f.residentComputer(t)
	if err := LockResidence(t.Context(), f.beginFence(t), f.EnvironmentID, computerID); err != nil {
		t.Fatal(err)
	}
	if !f.locked(t, "computers", computerID) || !f.locked(t, "computer_instances", instanceID) {
		t.Fatal("a residence did not lock the Computer and its Instance")
	}

	idle := f.insertComputer(t, "idle")
	if err := LockResidence(t.Context(), f.beginFence(t), f.EnvironmentID, idle); err != nil {
		t.Fatalf("a Computer without an Instance = %v", err)
	}
	if !f.locked(t, "computers", idle) {
		t.Fatal("a residence did not lock a Computer without an Instance")
	}
}

func TestAdmissionLocksTheLiveInstance(t *testing.T) {
	f := newFixture(t)
	if err := (Admission{}).LockLiveInstance(t.Context()); err == nil {
		t.Fatal("an unlocked admission locked an Instance")
	}

	computerID, instanceID := f.residentComputer(t)
	tx := f.beginFence(t)
	admission, err := LockForAdmission(t.Context(), tx, f.EnvironmentID, computerID)
	if err != nil {
		t.Fatal(err)
	}
	if f.locked(t, "computer_instances", instanceID) {
		t.Fatal("admission locked the Instance before LockLiveInstance")
	}
	if err := admission.LockLiveInstance(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !f.locked(t, "computer_instances", instanceID) {
		t.Fatal("LockLiveInstance did not lock the unreclaimed Instance")
	}

	idle := f.insertComputer(t, "idle")
	admission, err = LockForAdmission(t.Context(), f.beginFence(t), f.EnvironmentID, idle)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.LockLiveInstance(t.Context()); err != nil {
		t.Fatalf("a Computer without an Instance = %v", err)
	}
}

func TestSessionComputerFencesKeepTheirSessionPredicates(t *testing.T) {
	f := newFixture(t)
	work := f.AddRunLease(t, "running", time.Now())
	sessionID := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, work.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	ref := SessionComputerRef{EnvironmentID: f.EnvironmentID, ComputerID: computerID, SessionID: sessionID}
	other := ref
	other.SessionID = uuid.NewV7()
	for name, lock := range map[string]func(context.Context, pgx.Tx, SessionComputerRef) (db.Computer, error){
		"session": LockSessionComputer, "open session": LockOpenSessionComputer,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := lock(t.Context(), f.beginFence(t), other); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("another Session's fence = %v, want pgx.ErrNoRows", err)
			}
			locked, err := lock(t.Context(), f.beginFence(t), ref)
			if err != nil || locked.ID != pgvalue.UUID(computerID) {
				t.Fatalf("Session's Computer = %v, %v", locked.ID, err)
			}
			if !f.locked(t, "computers", computerID) {
				t.Fatal("the fence did not lock the Computer")
			}
		})
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET status='closing',close_sequence=0 WHERE id=$1`, sessionID)
	if _, err := LockOpenSessionComputer(t.Context(), f.beginFence(t), ref); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a closing Session's open fence = %v, want pgx.ErrNoRows", err)
	}
	if _, err := LockSessionComputer(t.Context(), f.beginFence(t), ref); err != nil {
		t.Fatalf("a closing Session's Computer = %v", err)
	}
}

func TestTokenWaitInstanceChecksTheComputerBeforeTheInstance(t *testing.T) {
	f := newFixture(t)
	computerID, instanceID := f.residentComputer(t)
	ref := TokenWaitInstanceRef{
		OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RegionID: runtest.Region,
		ComputerID: computerID, InstanceID: instanceID,
		Host:         Host{GroupID: runtest.WorkerGroupID, HostID: f.WorkerID, Epoch: 1},
		VMPlatformID: f.VMPlatformID, WriterGeneration: 2,
	}
	lock := func(ref TokenWaitInstanceRef) (db.ComputerInstance, error) {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		return LockTokenWaitInstance(t.Context(), tx, ref)
	}
	instance, err := lock(ref)
	if err != nil || instance.ID != pgvalue.UUID(instanceID) {
		t.Fatalf("token wait Instance = %v, %v", instance.ID, err)
	}

	otherPlatform := ref
	otherPlatform.VMPlatformID = "other-platform"
	if _, err := lock(otherPlatform); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another VM platform = %v, want pgx.ErrNoRows", err)
	}
	otherGeneration := ref
	otherGeneration.WriterGeneration = 3
	if _, err := lock(otherGeneration); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another writer generation = %v, want pgx.ErrNoRows", err)
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET desired_state='stopped' WHERE id=$1`, computerID)
	if _, err := LockTokenWaitInstance(t.Context(), f.beginFence(t), ref); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a Computer that is not desired active = %v, want pgx.ErrNoRows", err)
	}
	if f.locked(t, "computer_instances", instanceID) {
		t.Fatal("a rejected Computer locked its Instance")
	}
}
