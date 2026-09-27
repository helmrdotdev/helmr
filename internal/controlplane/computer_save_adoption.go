package controlplane

import (
	"bytes"
	"context"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

// adoptComputerSave acknowledges the host's durable local adoption and joined
// producers. Transfer remote source retention and release only this operation's
// pins atomically. Execution origins and the Computer head are not rewritten.
// A historical acknowledgement is evidence only; it grants no new mutation.
func (s *Server) adoptComputerSave(ctx context.Context, worker workerActor, request workerapi.ComputerSaveBeginRequest, root computer.GenerationRoot) error {
	params, err := computerSaveReceiptParams(worker, request)
	if err != nil {
		return err
	}
	fingerprint, err := computerSaveFingerprint(request, root)
	if err != nil {
		return err
	}

	validate := func(v db.ComputerDiskVersion) error {
		if !bytes.Equal(v.PublicationRequestFingerprint, fingerprint[:]) {
			return computerObjectConflict("source adoption differs from committed save")
		}
		return nil
	}
	replayed := func() (bool, error) {
		v, err := s.db.GetWorkerComputerSave(ctx, params)
		if err != nil {
			return false, err
		}
		if err = validate(v); err != nil {
			return false, err
		}
		// A committed save can leave its pending slot only through adoption.
		// The monotonic sequence excludes an unadmitted future request. This
		// remains true after later saves, without a second acknowledgement ledger.
		acknowledged, err := s.db.IsComputerInstanceSaveAdopted(ctx, db.IsComputerInstanceSaveAdoptedParams{ComputerInstanceID: params.ComputerInstanceID, EnvironmentID: params.EnvironmentID, WorkerHostID: params.WorkerHostID, WorkerGroupID: params.WorkerGroupID, WorkerEpoch: params.WorkerEpoch, WriterGeneration: params.WriterGeneration, Sequence: params.Sequence, SaveID: params.SaveID})
		return acknowledged.Valid && acknowledged.Bool, err
	}
	if done, err := replayed(); err != nil || done {
		return err
	}
	_, err = s.withComputerSave(ctx, worker, request, computerSaveAdopt, func(tx pgx.Tx, q *db.Queries, r db.ComputerInstance) error {
		v, err := q.GetWorkerComputerSave(ctx, params)
		if err != nil {
			return err
		}
		if err = validate(v); err != nil {
			return err
		}
		n, err := q.AdoptComputerInstanceSave(ctx, db.AdoptComputerInstanceSaveParams{ComputerInstanceID: r.ID, EnvironmentID: r.EnvironmentID, WriterGeneration: r.WriterGeneration, WriterTokenHash: r.WriterTokenHash, WorkerHostID: r.WorkerHostID, WorkerEpoch: r.WorkerEpoch, Sequence: request.Sequence, SaveID: v.ID})
		if err != nil {
			return err
		}
		if n != 1 {
			return computerObjectConflict("save source changed during adoption")
		}
		_, err = tx.Exec(ctx, `DELETE FROM computer_object_pins WHERE computer_instance_id=$1 AND publication_key=$2`, r.ID, computerSavePublicationKey(r))
		return err
	})
	if err != nil {
		if done, replayErr := replayed(); done && replayErr == nil {
			return nil
		}
	}
	return err
}
