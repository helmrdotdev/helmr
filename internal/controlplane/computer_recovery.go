package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Recovery completes at the same commit as the exact prepared Runtime's ready
// observation, before placement can grant customer execution authority.
func (s *Server) markRuntimeInstanceReady(ctx context.Context, params db.MarkRuntimeInstanceReadyParams) (db.RuntimeInstance, error) {
	var row db.RuntimeInstance
	err := s.inTx(ctx, func(work *txWork) error {
		tx, ok := work.tx.(pgx.Tx)
		if !ok {
			return errors.New("runtime readiness requires PostgreSQL authority")
		}
		var recovering bool
		var group pgtype.UUID
		if err := tx.QueryRow(ctx, `SELECT coalesce(c.recovery_runtime_id=r.id AND c.recovery_completed_at IS NULL,false),r.worker_group_id
			FROM runtime_instances r JOIN computers c ON c.id=r.workspace_id
			WHERE r.id=$1 AND r.worker_instance_id=$2 AND r.worker_epoch=$3`, params.ID, params.WorkerInstanceID, params.WorkerEpoch).Scan(&recovering, &group); err != nil {
			return err
		}
		if recovering {
			_, err := dispatch.LockComputerSourcePreparation(ctx, tx, dispatch.ComputerPreparationFence{
				RuntimeID: params.ID, WorkerID: params.WorkerInstanceID, WorkerGroupID: group,
				WorkerEpoch: params.WorkerEpoch, DesiredVersion: params.DesiredVersion,
			})
			if err != nil {
				return err
			}
		}
		var err error
		row, err = work.q.MarkRuntimeInstanceReady(ctx, params)
		if err != nil {
			return err
		}
		if recovering {
			result, err := tx.Exec(ctx, `UPDATE computers SET recovery_completed_at=$3,revision=revision+1,updated_at=$3
				WHERE id=$1 AND recovery_runtime_id=$2 AND recovery_completed_at IS NULL
				AND recovery_version_id=$4`, row.WorkspaceID, row.ID, row.ReadyAt, row.RetainedComputerSourceVersionID)
			if err != nil {
				return err
			}
			if result.RowsAffected() != 1 {
				return pgx.ErrNoRows
			}
		}
		return nil
	})
	return row, err
}
