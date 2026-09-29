package dispatch

import (
	"errors"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

func TestComputerInitialPreparationNeedsNoMember(t *testing.T) {
	f := runtest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.EnvironmentID, f.DeploymentID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, f.WorkerID)
	c, err := db.New(f.Pool).CreateComputerFromCurrentDeployment(t.Context(), db.CreateComputerFromCurrentDeploymentParams{ID: pgvalue.UUID(uuid.NewV7()), InitialVersionID: pgvalue.UUID(uuid.NewV7()), OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), DeploymentDefinitionID: pgvalue.UUID(f.ComputerDefinitionID), SandboxDeclaredID: "test-computer"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	p, err := discoverComputerPlacement(t.Context(), tx, c.EnvironmentID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err = lockComputerPlacement(t.Context(), tx, p)
	if err != nil {
		t.Fatal(err)
	}
	i, err := a.allocateComputerPlacement(t.Context(), tx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	fence := ComputerPreparationFence{RuntimeID: i.ID, WorkerID: i.WorkerHostID, WorkerGroupID: i.WorkerGroupID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion}
	owner, err := LockComputerPreparation(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if owner.VersionID != c.HeadDiskVersionID || owner.WriterGeneration != 1 {
		t.Fatalf("preparation=%+v", owner)
	}
	if _, err = LockComputerSourcePreparation(t.Context(), tx, fence); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unpublished source authorized: %v", err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, i.ID)
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = LockComputerPreparation(t.Context(), tx, fence); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired writer authorized: %v", err)
	}
}

func TestComputerSourcePreparationRechecksDeadline(t *testing.T) {
	f, work, _ := commandPlacementFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_version=0,observed_desired_version=0,ready_at=NULL WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	var fence ComputerPreparationFence
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.worker_host_id,i.worker_group_id,i.worker_epoch,i.desired_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, work.LeaseID).Scan(&fence.RuntimeID, &fence.WorkerID, &fence.WorkerGroupID, &fence.WorkerEpoch, &fence.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	owner, err := LockComputerSourcePreparation(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = LockComputerPreparation(t.Context(), tx, fence); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("committed source initialized again: %v", err)
	}
	// Time may pass during certification even though this transaction owns the locks.
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET preparation_expires_at=$2 WHERE id=$1`, fence.RuntimeID, time.Now().Add(-time.Second))
	if err = owner.CheckDeadlines(t.Context(), tx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired preparation committed: %v", err)
	}
}
