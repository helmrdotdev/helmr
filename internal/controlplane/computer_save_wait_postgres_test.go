package controlplane

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestComputerSaveContinuesAcrossMemberWait(t *testing.T) {
	for _, admission := range []string{"open", "draining"} {
		t.Run(admission, func(t *testing.T) {
			f, member, worker, request := instanceSaveFixture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state=$2 WHERE id=$1`, request.ComputerInstanceID, admission)
			execute := func(op computerSaveOperation) error {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback(t.Context())
				_, err = applyComputerSave(t.Context(), tx, worker, request, op, func(_ pgx.Tx, q *db.Queries, i db.ComputerInstance) error {
					if op != computerSaveAbandon {
						return nil
					}
					params, err := computerSaveReceiptParams(worker, request)
					if err != nil {
						return err
					}
					n, err := q.AbandonComputerInstanceSave(t.Context(), db.AbandonComputerInstanceSaveParams{ComputerInstanceID: i.ID, EnvironmentID: i.EnvironmentID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, WriterGeneration: i.WriterGeneration, WriterTokenHash: i.WriterTokenHash, Sequence: request.Sequence, SaveID: params.SaveID})
					if err == nil && n != 1 {
						return errors.New("save slot was not released")
					}
					return err
				})
				if err == nil {
					err = tx.Commit(t.Context())
				}
				return err
			}
			if err := execute(computerSaveBegin); err != nil {
				t.Fatal(err)
			}
			// Member waiting does not freeze the shared physical writer.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='waiting',active_started_at=NULL WHERE id=$1`, member.RunID)
			if err := execute(computerSaveBegin); err != nil {
				t.Fatalf("pending replay during wait: %v", err)
			}
			if err := execute(computerSaveWrite); err != nil {
				t.Fatalf("pending write during wait: %v", err)
			}
			if err := execute(computerSaveAbandon); err != nil {
				t.Fatal(err)
			}
			request.SaveID = uuid.NewV7().String()
			request.Sequence++
			if err := execute(computerSaveBegin); err != nil {
				t.Fatalf("next save during wait: %v", err)
			}
		})
	}
}

func TestComputerSaveRejectsSealedInstance(t *testing.T) {
	for _, admission := range []string{"checkpointing", "closed"} {
		t.Run(admission, func(t *testing.T) {
			f, _, worker, request := instanceSaveFixture(t)
			// Even after the host observes the desired version, capture admission
			// cannot reopen live disk publication from a frozen Computer.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state=$2 WHERE id=$1`, request.ComputerInstanceID, admission)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			_, err = applyComputerSave(t.Context(), tx, worker, request, computerSaveBegin, nil)
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("sealed writer save: %v", err)
			}
		})
	}
}
