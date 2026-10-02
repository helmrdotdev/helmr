package dispatch

import (
	"bytes"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// Assigning a Computer allocates an Instance whose initial preparation needs
// no member: the computer owner delivers its initial key, it has no source
// to restore, and an expired writer holds no preparation.
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
	p, err := discoverInstanceAssignment(t.Context(), tx, c.EnvironmentID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err = lockInstanceAssignment(t.Context(), tx, p)
	if err != nil {
		t.Fatal(err)
	}
	i, err := a.allocateInstanceAssignment(t.Context(), tx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if i.WriterGeneration != 1 {
		t.Fatalf("allocated writer generation = %d", i.WriterGeneration)
	}
	local, err := computerkey.NewLocal("local-1", bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	broker, err := computer.NewKeyBroker(f.Pool, local)
	if err != nil {
		t.Fatal(err)
	}
	principal := workergroup.HostPrincipal{HostID: pgvalue.MustUUIDValue(i.WorkerHostID), GroupID: pgvalue.MustUUIDValue(i.WorkerGroupID), Epoch: i.WorkerEpoch}
	if err = f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,g.claim_version FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1`, i.WorkerHostID).Scan(&principal.HostClaimVersion, &principal.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	ref := computer.PreparationRef{InstanceID: pgvalue.MustUUIDValue(i.ID), DesiredVersion: i.DesiredVersion}
	material, err := broker.PrepareSeed(t.Context(), principal, ref)
	if err != nil {
		t.Fatalf("initial preparation: %v", err)
	}
	clear(material.Key.Key)
	if _, err = broker.SourceKeys(t.Context(), principal, ref); !errors.Is(err, computer.ErrAuthorityChanged) {
		t.Fatalf("unpublished source authorized: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, i.ID)
	if _, err = broker.PrepareSeed(t.Context(), principal, ref); !errors.Is(err, computer.ErrAuthorityChanged) {
		t.Fatalf("expired writer authorized: %v", err)
	}
}
