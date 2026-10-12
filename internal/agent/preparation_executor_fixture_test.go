package agent_test

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Fixture allocation isolates executor authority tests from supply selection.
// Production allocation must go through dispatch.Allocator and its capacity transaction.
func claimPreparationFixture(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, ref computer.ImageBuildExecutor) (time.Time, error) {
	if !ref.Valid() || ref.Epoch != 1 {
		return time.Time{}, agent.ErrInvalidInput
	}
	var expires time.Time
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		var available bool
		if err := tx.QueryRow(ctx, `SELECT h.status='active' AND g.status='active' FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, host.HostID).Scan(&available); err != nil {
			return err
		}
		if !available {
			return agent.ErrNotReady
		}
		p, err := computer.ReadImageBuild(ctx, tx, ref.EnvironmentID, ref.ImageBuildID)
		if err != nil {
			return err
		}
		if err = computer.LockImageSpec(ctx, tx, p.EnvironmentID, p.SpecID); err != nil {
			return err
		}
		var claimed bool
		var state string
		var current bool
		if err = tx.QueryRow(ctx, `SELECT worker_host_id IS NOT NULL,status,deadline_at>clock_timestamp() FROM image_builds WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, p.EnvironmentID, p.ID).Scan(&claimed, &state, &current); err != nil {
			return err
		}
		if claimed {
			if err = computer.CheckImageBuildExecutor(ctx, tx, host, ref, true); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT executor_expires_at FROM image_builds WHERE environment_id=$1 AND id=$2`, p.EnvironmentID, p.ID).Scan(&expires)
		}
		if state != "queued" || !current {
			return agent.ErrNotReady
		}
		digest := sha256.Sum256(ref.ChannelCredential)
		return tx.QueryRow(ctx, `UPDATE image_builds SET status='running',executor_epoch=$3,worker_host_id=$4,worker_epoch=$5,allocation_id=$6,channel_credential_digest=$7,
    delivered_at=clock_timestamp(),reserved_cpu_millis=1000,reserved_memory_bytes=536870912,
    reserved_scratch_bytes=(SELECT per_vm_guest_ephemeral_disk_bytes FROM worker_hosts WHERE id=$4),
    vm_platform_id=(SELECT vm_platform_id FROM worker_hosts WHERE id=$4),vm_vcpu_count=1,
    cpu_config_digest=(SELECT cpu_environment_digest FROM worker_hosts WHERE id=$4),
    executor_expires_at=LEAST(deadline_at,clock_timestamp()+interval '1 minute') WHERE environment_id=$1 AND id=$2 RETURNING executor_expires_at`, p.EnvironmentID, p.ID, ref.Epoch, host.HostID, host.Epoch, ref.InstanceID, digest[:]).Scan(&expires)
	})
	if err != nil {
		return time.Time{}, agent.HideMissing(err)
	}
	return expires, nil
}
