package agent

import (
	"bytes"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestComputerCheckpointReadRequiresExactLiveRestoreAllocation(t *testing.T) {
	for _, change := range []string{"none", "instance", "host", "epoch", "environment", "undelivered", "expired", "consumed", "aborting", "corrupt", "revoked"} {
		t.Run(change, func(t *testing.T) {
			f := newComputerRestoreFixture(t)
			id := ComputerLeaseIdentity{EnvironmentID: f.f.env, ComputerID: f.f.computer, Epoch: f.epoch}
			if err := f.f.pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, id.EnvironmentID, id.ComputerID, id.Epoch).Scan(&id.InstanceID); err != nil {
				t.Fatal(err)
			}
			host := f.host
			switch change {
			case "instance":
				id.InstanceID = uuid.NewV7()
			case "host":
				host = *f.f.host()
			case "epoch":
				id.Epoch++
			case "environment":
				id.EnvironmentID = uuid.NewV7()
			case "undelivered":
				dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_leases SET delivered_at=NULL,expires_at=NULL WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, id.EnvironmentID, id.ComputerID, id.Epoch)
			case "expired":
				dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, id.EnvironmentID, id.ComputerID, id.Epoch)
			case "consumed", "aborting":
				dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_checkpoints SET status=$2 WHERE id=$1`, f.manifest.CheckpointID, change)
			case "corrupt":
				dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_checkpoints SET manifest=decode('00','hex') WHERE id=$1`, f.manifest.CheckpointID)
			case "revoked":
				dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computers SET integrity_fault_at=clock_timestamp(),integrity_fault_reason='test' WHERE environment_id=$1 AND id=$2`, id.EnvironmentID, id.ComputerID)
			}
			got, err := ReadComputerCheckpoint(t.Context(), f.f.pool, host, id)
			if change != "none" {
				if err == nil {
					t.Fatalf("%s exposed checkpoint", change)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want, _ := f.manifest.Encode()
			actual, _ := got.Encode()
			if !bytes.Equal(want, actual) {
				t.Fatal("checkpoint manifest changed")
			}
			p := f.prepare(t)
			if _, err := ReadComputerCheckpoint(t.Context(), f.f.pool, host, id); err != nil {
				t.Fatalf("prepared target retry: %v", err)
			}
			if err := ValidateComputerRestore(t.Context(), f.f.pool, host, id.EnvironmentID, p, restoreReceipt(p, false, false)); err != nil {
				t.Fatal(err)
			}
			if err := CommitComputerRestore(t.Context(), f.f.pool, host, id.EnvironmentID, p, restoreReceipt(p, true, false)); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadComputerCheckpoint(t.Context(), f.f.pool, host, id); err == nil {
				t.Fatal("committed image became readable for a second physical attempt")
			}
		})
	}
}
