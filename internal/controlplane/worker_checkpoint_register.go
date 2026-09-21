package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerRegisterCheckpoint(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RegisterCheckpointRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	response, err := s.registerCheckpoint(r.Context(), workerFromContext(r.Context()), request)
	if err != nil {
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, pgx.ErrNoRows) {
			writeError(w, conflict(errors.New("checkpoint registration is stale or differs from its candidate")))
			return
		}
		if isDeterministicWorkerAdmission(err) {
			writeError(w, conflict(errors.New("checkpoint candidate conflicts with retained storage authority")))
			return
		}
		if errorStatus(err) < 500 {
			writeError(w, err)
			return
		}
		s.log.Error("register checkpoint failed", "error", err)
		writeError(w, errors.New("register checkpoint"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) registerCheckpoint(ctx context.Context, worker workerActor, request workerapi.RegisterCheckpointRequest) (workerapi.CheckpointResponse, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return workerapi.CheckpointResponse{}, badRequest(err)
	}
	waitID, err := parseCanonicalUUID("run_wait_id", request.RunWaitID)
	if err != nil {
		return workerapi.CheckpointResponse{}, badRequest(err)
	}
	checkpointID, err := parseCanonicalUUID("checkpoint_id", request.CheckpointID)
	if err != nil {
		return workerapi.CheckpointResponse{}, badRequest(err)
	}
	if request.RequestVersion <= 0 {
		return workerapi.CheckpointResponse{}, badRequest(errors.New("request_version must be positive"))
	}
	disk := request.Manifest.RuntimeState.Computer
	if disk == nil {
		return workerapi.CheckpointResponse{}, badRequest(errors.New("checkpoint requires a Computer disk"))
	}
	computerID, err := parseCanonicalUUID("computer_id", disk.ComputerID)
	if err != nil {
		return workerapi.CheckpointResponse{}, badRequest(err)
	}
	descriptor := computer.DiskArtifact{Object: cas.Descriptor{Digest: disk.Artifact.Digest, SizeBytes: disk.Artifact.SizeBytes, MediaType: disk.Artifact.MediaType}, LogicalBytes: disk.LogicalBytes}
	if err := descriptor.Validate(disk.LogicalBytes); err != nil {
		return workerapi.CheckpointResponse{}, badRequest(err)
	}
	// Timings are observations, not candidate identity; uploads have not happened.
	request.Manifest.Phases = nil
	encoded, proofs, err := validateCheckpointManifest(request.Manifest, request.CheckpointID, request.Manifest.RecoveryPoint.RunID, request.Manifest.RecoveryPoint.AttemptNumber, request.RunWaitID, request.Manifest.RecoveryPoint.Runtime.ID)
	if err != nil {
		return workerapi.CheckpointResponse{}, badRequest(err)
	}
	objects := []checkpointArtifactProof{{role: "computer", artifact: disk.Artifact}}
	for _, proof := range proofs.all() {
		objects = append(objects, proof)
	}
	digests := make(map[string]bool, len(objects))
	for _, object := range objects {
		if digests[object.artifact.Digest] {
			return workerapi.CheckpointResponse{}, badRequest(errors.New("checkpoint objects must have distinct encrypted identities"))
		}
		digests[object.artifact.Digest] = true
	}
	var runID uuid.UUID
	err = s.inTx(ctx, func(work *txWork) error {
		source, err := lockCheckpointSource(ctx, work, worker, lease, request.Lease.LeaseSequence, waitID, checkpointID, request.RequestVersion, request.Manifest)
		if err != nil {
			return err
		}
		if source.authority.workspace.ID != pgvalue.UUID(computerID) {
			return errStaleRunLeaseClaim
		}
		if err := descriptor.Validate(source.authority.runtime.ReservedGuestEphemeralDiskBytes); err != nil {
			return errStaleRunLeaseClaim
		}
		n, err := work.q.RegisterCheckpointManifest(ctx, db.RegisterCheckpointManifestParams{ID: pgvalue.UUID(checkpointID), Manifest: encoded})
		if err != nil {
			return err
		}
		if n != 1 {
			return errStaleRunLeaseClaim
		}
		for _, object := range objects {
			if _, err := work.q.RegisterCheckpointObject(ctx, db.RegisterCheckpointObjectParams{CheckpointID: pgvalue.UUID(checkpointID), Role: object.role, Digest: object.artifact.Digest, SizeBytes: object.artifact.SizeBytes, MediaType: object.artifact.MediaType}); err != nil {
				return fmt.Errorf("register checkpoint %s: %w", object.role, err)
			}
		}
		// Registration can block behind another writer. Recheck expiry using DB
		// time after the writes before releasing the transaction's authority.
		now, err := work.q.GetRunLeaseRenewalTime(ctx)
		if err != nil {
			return err
		}
		if !now.Valid || !now.Time.Before(source.authority.runLease.ExpiresAt.Time) || (source.expiresAt.Valid && !now.Time.Before(source.expiresAt.Time)) {
			return errStaleRunLeaseClaim
		}
		runID = uuid.UUID(source.authority.run.ID.Bytes)
		return nil
	})
	if err != nil {
		return workerapi.CheckpointResponse{}, err
	}
	return workerapi.CheckpointResponse{RunID: runID.String(), RunWaitID: waitID.String(), CheckpointID: checkpointID.String()}, nil
}
