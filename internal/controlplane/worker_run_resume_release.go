package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

type runResumeReleaseProof struct {
	runWaitID            pgtype.UUID
	checkpointID         pgtype.UUID
	resumeAttachID       pgtype.UUID
	resumeRequestVersion int64
}

func (s *Server) workerAcknowledgeRunResumeRelease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.RunResumeReleaseRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker Run resume release request JSON: %w", err)))
		return
	}
	parsedLease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	proof, err := parseRunResumeReleaseProof(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}

	receipt, err := s.acknowledgeRunResumeRelease(
		r.Context(), workerFromContext(r.Context()), pgvalue.UUID(parsedLease.leaseID), request.Lease, proof,
	)
	if err != nil {
		if errors.Is(err, errStaleRunLeaseClaim) {
			writeError(w, conflict(errors.New("Run resume release acknowledgement is stale")))
			return
		}
		s.log.Error("acknowledge Run resume release failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("acknowledge Run resume release"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.RunResumeReleaseResponse{
		Lease: receipt, RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID,
		ResumeAttachID: request.ResumeAttachID, ResumeRequestVersion: request.ResumeRequestVersion,
	})
}

func parseRunResumeReleaseProof(request workerapi.RunResumeReleaseRequest) (runResumeReleaseProof, error) {
	parseID := func(name, raw string) (pgtype.UUID, error) {
		value, err := ids.Parse(raw)
		if err != nil {
			return pgtype.UUID{}, fmt.Errorf("%s must be a canonical UUIDv7", name)
		}
		return pgvalue.UUID(value), nil
	}
	runWaitID, err := parseID("run_wait_id", request.RunWaitID)
	if err != nil {
		return runResumeReleaseProof{}, err
	}
	checkpointID, err := parseID("checkpoint_id", request.CheckpointID)
	if err != nil {
		return runResumeReleaseProof{}, err
	}
	resumeAttachID, err := parseID("resume_attach_id", request.ResumeAttachID)
	if err != nil {
		return runResumeReleaseProof{}, err
	}
	if request.ResumeRequestVersion <= 0 {
		return runResumeReleaseProof{}, errors.New("resume_request_version must be positive")
	}
	return runResumeReleaseProof{
		runWaitID: runWaitID, checkpointID: checkpointID, resumeAttachID: resumeAttachID,
		resumeRequestVersion: request.ResumeRequestVersion,
	}, nil
}

func (s *Server) acknowledgeRunResumeRelease(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	expected workerapi.RunLeaseFence,
	proof runResumeReleaseProof,
) (workerapi.RunLeaseFence, error) {
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetRunLeaseStartLocators(ctx, db.GetRunLeaseStartLocatorsParams{
			ID: leaseID, LeaseSequence: expected.LeaseSequence, WorkerGroupID: worker.WorkerGroupID,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		mode := deriveRunStartMode(locators)
		if mode != runLeaseClaimRestore && mode != runLeaseClaimAttachParent {
			return errStaleRunLeaseClaim
		}
		authority, err := lockRunStartAuthority(
			ctx, work.q, worker, leaseID, expected.LeaseSequence, locators, mode,
		)
		if err != nil {
			return err
		}
		if authority.run.Status != db.RunStatusRunning || authority.runLease.State != db.RunLeaseStateRunning {
			return errStaleRunLeaseClaim
		}
		if err := validateRunStartArm(runStartArm{
			mode: mode, runWaitID: proof.runWaitID,
			checkpointID: proof.checkpointID, resumeAttachID: proof.resumeAttachID,
			resumeRequestVersion: proof.resumeRequestVersion,
		}, runStartValidationAuthority{
			run: authority.run, parentRun: authority.parentRun, runLease: authority.runLease,
			runtime: authority.runtime, workspace: authority.workspace,
			workspaceMount: authority.workspaceMount, runWait: authority.runWait,
		}); err != nil {
			return err
		}
		wait := authority.runWait
		if wait.ID != proof.runWaitID ||
			wait.CurrentRunLeaseID != authority.runLease.ID ||
			wait.ResumeAttachID != proof.resumeAttachID ||
			wait.ResumeRequestVersion != proof.resumeRequestVersion {
			return errStaleRunLeaseClaim
		}
		checkpointID := wait.SuspendCheckpointID
		checkpointKind := db.RunCheckpointKindSuspend
		if wait.ChildRunID.Valid &&
			wait.ChildParentOwned.Valid &&
			wait.ChildParentOwned.Bool &&
			wait.ConditionState == db.WaitStateCompleted {
			checkpointID = wait.HandoffResumeCheckpointID
			checkpointKind = db.RunCheckpointKindHandoffResume
		}
		if checkpointID != proof.checkpointID ||
			(mode == runLeaseClaimRestore && authority.runtime.RestoreCheckpointID != proof.checkpointID) {
			return errStaleRunLeaseClaim
		}
		if wait.SuspensionState == db.RunWaitStateReleased &&
			wait.ResumeAckVersion == proof.resumeRequestVersion {
			return nil
		}
		if _, err := work.q.LockReadyRunCheckpoint(ctx, db.LockReadyRunCheckpointParams{
			ID: proof.checkpointID, Kind: checkpointKind,
			RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
			RunWaitID: wait.ID, WorkspaceID: authority.workspace.ID,
		}); err != nil {
			return staleRunLeaseClaim(err)
		}
		if wait.SuspensionState != db.RunWaitStateResuming ||
			wait.ResumeAckVersion >= proof.resumeRequestVersion {
			return errStaleRunLeaseClaim
		}
		_, err = work.q.ReleaseRunResumeWait(ctx, db.ReleaseRunResumeWaitParams{
			ID: wait.ID, EnvironmentID: wait.EnvironmentID, RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
			CurrentRunLeaseID: authority.runLease.ID, CheckpointID: proof.checkpointID,
			ResumeAttachID: proof.resumeAttachID, ResumeRequestVersion: proof.resumeRequestVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		return nil
	})
	if err != nil {
		return workerapi.RunLeaseFence{}, err
	}
	return expected, nil
}
