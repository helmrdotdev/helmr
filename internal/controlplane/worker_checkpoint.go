package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerMarkCheckpointReady(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CheckpointReadyRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	for name, value := range map[string]string{"computer_instance_id": request.ComputerInstanceID, "checkpoint_id": request.CheckpointID} {
		if _, err := parseCanonicalUUID(name, value); err != nil {
			writeError(w, badRequest(err))
			return
		}
	}
	if request.WorkerEpoch <= 0 || request.DesiredVersion <= 0 {
		writeError(w, badRequest(errors.New("checkpoint source versions must be positive")))
		return
	}
	response, err := s.commitCheckpointReady(r.Context(), workerFromContext(r.Context()), request)
	if errors.Is(err, dispatch.ErrCheckpointCandidate) {
		writeError(w, badRequest(err))
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("checkpoint ready source or candidate changed")))
		return
	}
	if err != nil {
		s.writeComputerPublicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) commitCheckpointReady(ctx context.Context, worker workerActor, request workerapi.CheckpointReadyRequest) (workerapi.ComputerCheckpointResponse, error) {
	authority := dispatch.ComputerCaptureWorker{GroupID: pgvalue.UUID(worker.WorkerGroupID), HostID: pgvalue.UUID(worker.WorkerHostID), Epoch: worker.WorkerEpoch}
	candidate := workerapi.RegisterCheckpointRequest(request)
	var checkpoint db.ComputerCheckpoint
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		checkpoint, err = dispatch.CheckComputerCheckpointReady(ctx, work.tx, authority, candidate)
		return err
	})
	if err != nil {
		return workerapi.ComputerCheckpointResponse{}, err
	}
	// A committed receipt is independent of current storage availability. Every
	// replay still checks the authenticated source and exact candidate identity.
	if checkpoint.Status != "ready" {
		if s.cas == nil {
			return workerapi.ComputerCheckpointResponse{}, unavailable(errors.New("checkpoint storage unavailable"))
		}
		var manifest workerapi.CheckpointManifest
		if err = json.Unmarshal(checkpoint.Manifest, &manifest); err != nil {
			return workerapi.ComputerCheckpointResponse{}, err
		}
		if len(manifest.RuntimeState.MemoryArtifacts) != 1 {
			return workerapi.ComputerCheckpointResponse{}, dispatch.ErrCheckpointCandidate
		}
		descriptors := []workerapi.CheckpointArtifact{manifest.RuntimeState.ConfigArtifact, manifest.RuntimeState.VMStateArtifact, manifest.RuntimeState.MemoryArtifacts[0], manifest.RuntimeState.ScratchDiskArtifact}
		observed := make([]cas.Object, 0, len(descriptors))
		for _, d := range descriptors {
			object, e := s.cas.Stat(ctx, d.Digest)
			if e != nil {
				return workerapi.ComputerCheckpointResponse{}, unavailable(errors.New("checkpoint object unavailable"))
			}
			observed = append(observed, object)
		}
		err = s.inTx(ctx, func(work *txWork) error {
			var err error
			checkpoint, err = dispatch.CompleteComputerCheckpoint(ctx, work.tx, authority, candidate, observed)
			return err
		})
		if err != nil {
			return workerapi.ComputerCheckpointResponse{}, err
		}
	}
	return workerapi.ComputerCheckpointResponse{ComputerInstanceID: request.ComputerInstanceID, WorkerEpoch: request.WorkerEpoch, DesiredVersion: request.DesiredVersion, CheckpointID: request.CheckpointID, ComputerDiskVersionID: pgvalue.UUIDString(checkpoint.PrivateComputerDiskVersionID)}, nil
}
