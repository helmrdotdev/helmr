package controlplane

import (
	"context"
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerComputerRestorePlan(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerRestorePlanRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if _, err := parseCanonicalUUID("environment_id", request.EnvironmentID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if _, err := parseCanonicalUUID("computer_instance_id", request.ComputerInstanceID); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.WriterGeneration <= 0 {
		writeError(w, badRequest(errors.New("positive writer generation is required")))
		return
	}
	var plan *workerapi.ComputerRestorePlan
	err := s.inTx(r.Context(), func(work *txWork) error {
		var err error
		plan, err = loadComputerRestorePlan(r.Context(), work.tx, workerFromContext(r.Context()), request, s.computerFencingKey)
		return err
	})
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("Computer restore authority changed")))
		return
	}
	if err != nil {
		s.log.Error("load Computer restore plan", "error", err)
		writeError(w, errors.New("load Computer restore plan"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerRestorePlanResponse{Plan: plan})
}

// The Computer server owns this physical channel. Refresh its writer under the same
// authority locks before projecting the committed, still-unstarted member set.
func loadComputerRestorePlan(ctx context.Context, tx pgx.Tx, worker workerActor, request workerapi.ComputerRestorePlanRequest, key disk.FencingKey) (*workerapi.ComputerRestorePlan, error) {
	i, err := renewComputerInstance(ctx, tx, worker, workerapi.ComputerInstanceRenewRequest{EnvironmentID: request.EnvironmentID, ComputerInstanceID: request.ComputerInstanceID, WriterGeneration: request.WriterGeneration})
	if err != nil {
		return nil, err
	}
	if i.DesiredState != "ready" || i.ObservedState != "ready" || i.ObservedDesiredVersion != i.DesiredVersion || i.AdmissionState != "restoring" || !i.SourceCheckpointID.Valid {
		return nil, pgx.ErrNoRows
	}
	var committed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_checkpoints c JOIN control_outbox o ON o.topic='computer.restore.activate' AND o.payload->>'computer_instance_id'=($2::uuid)::text AND o.payload->>'checkpoint_id'=c.id::text AND o.payload->>'desired_version'=($3::bigint)::text AND o.payload->>'writer_generation'=($4::bigint)::text WHERE c.id=$1 AND c.status='ready' AND c.resume_computer_instance_id=$2 AND c.resume_committed_at IS NOT NULL AND (c.expires_at IS NULL OR c.expires_at>clock_timestamp()) AND o.status IN ('pending','claimed'))`, i.SourceCheckpointID, i.ID, i.DesiredVersion, i.WriterGeneration).Scan(&committed); err != nil {
		return nil, err
	}
	if !committed {
		return nil, nil
	}
	capability, err := deriveComputerCapability(key, i)
	if err != nil {
		return nil, err
	}
	plan := &workerapi.ComputerRestorePlan{ComputerInstanceID: pgvalue.UUIDString(i.ID), ComputerID: pgvalue.UUIDString(i.ComputerID), CheckpointID: pgvalue.UUIDString(i.SourceCheckpointID), DesiredVersion: i.DesiredVersion, WriterGeneration: i.WriterGeneration, WorkerHostID: pgvalue.UUIDString(i.WorkerHostID), WorkerEpoch: i.WorkerEpoch, VMPlatformID: i.VMPlatformID, WriteCapability: capability.Token, Members: []workerapi.ComputerRestoreMember{}}
	rows, err := tx.Query(ctx, `SELECT m.run_id::text,m.attempt_number,l.id::text,l.lease_sequence,a.base_computer_disk_version_id::text,l.expires_at FROM computer_checkpoint_runs m JOIN runs r ON r.id=m.run_id JOIN run_attempts a ON a.run_id=m.run_id AND a.number=m.attempt_number JOIN run_leases l ON l.id=r.current_run_lease_id AND l.computer_instance_id=$2 AND l.attempt_number=m.attempt_number JOIN run_waits w ON w.id=m.run_wait_id WHERE m.checkpoint_id=$1 AND r.status='waiting' AND r.current_attempt_number=m.attempt_number AND l.status='assigned' AND l.writer_generation=$3 AND l.expires_at>clock_timestamp() AND l.start_deadline_at>clock_timestamp() AND w.suspension_status='resuming' AND w.current_run_lease_id=l.id AND w.prior_run_lease_id=m.source_run_lease_id ORDER BY m.run_id FOR UPDATE OF l`, i.SourceCheckpointID, i.ID, i.WriterGeneration)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var member workerapi.ComputerRestoreMember
		if err := rows.Scan(&member.RunID, &member.AttemptNumber, &member.Lease.ID, &member.Lease.LeaseSequence, &member.BaseComputerDiskVersionID, &member.ExpiresAt); err != nil {
			rows.Close()
			return nil, err
		}
		plan.Members = append(plan.Members, member)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Time predicates in a locking SELECT may have been evaluated before it
	// waited for a lease lock. Recheck the entire set after acquiring those locks.
	var live bool
	if err := tx.QueryRow(ctx, `SELECT i.writer_expires_at>clock_timestamp()
 AND (c.expires_at IS NULL OR c.expires_at>clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM run_leases l WHERE l.computer_instance_id=i.id
 AND l.id IN (SELECT w.current_run_lease_id FROM computer_checkpoint_runs m JOIN run_waits w ON w.id=m.run_wait_id WHERE m.checkpoint_id=c.id)
 AND (l.expires_at<=clock_timestamp() OR l.start_deadline_at<=clock_timestamp()))
 FROM computer_instances i JOIN computer_checkpoints c ON c.id=i.source_checkpoint_id WHERE i.id=$1`, i.ID).Scan(&live); err != nil {
		return nil, err
	}
	if !live {
		return nil, pgx.ErrNoRows
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM computer_checkpoint_runs WHERE checkpoint_id=$1`, i.SourceCheckpointID).Scan(&count); err != nil {
		return nil, err
	}
	if len(plan.Members) != count {
		return nil, pgx.ErrNoRows
	}
	return plan, nil
}
