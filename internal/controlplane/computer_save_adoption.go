package controlplane

import (
	"bytes"
	"context"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// adoptComputerSave acknowledges the host's durable local adoption and joined
// producers. Transfer remote source retention and release only this operation's
// pins atomically. Execution origins and the Computer head are not rewritten.
// A historical acknowledgement is evidence only; it grants no new mutation.
func (s *Server) adoptComputerSave(ctx context.Context, worker workerActor, request workerapi.ComputerSaveBeginRequest, root computer.GenerationRoot) error {
	id, err := parseCanonicalUUID("save_id", request.SaveID)
	if err != nil {
		return err
	}
	fingerprint, err := computerSaveFingerprint(request, root)
	if err != nil {
		return err
	}
	params := db.GetWorkerComputerSaveParams{SaveID: pgvalue.UUID(id), Sequence: pgtype.Int8{Int64: request.Sequence, Valid: true}, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerEpoch: worker.WorkerEpoch}
	validate := func(v db.ComputerVersion) error {
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
		acknowledged, err := s.db.IsRuntimeComputerSaveAdopted(ctx, db.IsRuntimeComputerSaveAdoptedParams{RuntimeInstanceID: v.PublisherRuntimeInstanceID, Sequence: v.PublisherSaveSequence.Int64, SaveID: v.ID})
		return acknowledged.Valid && acknowledged.Bool, err
	}
	if done, err := replayed(); err != nil || done {
		return err
	}
	_, err = s.withComputerSave(ctx, worker, request, computerSaveAdopt, func(tx pgx.Tx, q *db.Queries, r db.RuntimeInstance, l db.WorkspaceLease) error {
		v, err := q.GetWorkerComputerSave(ctx, params)
		if err != nil {
			return err
		}
		if err = validate(v); err != nil {
			return err
		}
		n, err := q.AdoptRuntimeComputerSave(ctx, db.AdoptRuntimeComputerSaveParams{RuntimeInstanceID: r.ID, WorkerInstanceID: r.WorkerInstanceID, WorkerEpoch: r.WorkerEpoch, Sequence: request.Sequence, SaveID: v.ID, LeaseID: l.ID})
		if err != nil {
			return err
		}
		if n != 1 {
			return computerObjectConflict("save source changed during adoption")
		}
		_, err = tx.Exec(ctx, `DELETE FROM runtime_computer_object_pins WHERE runtime_instance_id=$1 AND publication_key=$2`, r.ID, computerSavePublicationKey(r))
		return err
	})
	if err != nil {
		if done, replayErr := replayed(); done && replayErr == nil {
			return nil
		}
	}
	return err
}
