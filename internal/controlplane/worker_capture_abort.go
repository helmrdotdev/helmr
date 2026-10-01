package controlplane

import (
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerAbortCapture(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CaptureAbortRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	ref, err := checkpointRef(workerFromContext(r.Context()), request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion, request.CheckpointID)
	if err != nil {
		writeError(w, err)
		return
	}
	plan, err := computer.AbortCapture(r.Context(), s.tx, s.computerFencingKey, ref)
	if err != nil {
		s.writeWorkerComputerError(w, err, computerCaptureAbortOperation, "abort Computer capture")
		return
	}
	i, cp := plan.Instance, plan.Checkpoint
	response := workerapi.CaptureAbortResponse{ComputerInstanceID: request.ComputerInstanceID, ComputerID: pgvalue.UUIDString(i.ComputerID), WorkerHostID: pgvalue.UUIDString(i.WorkerHostID), WorkerEpoch: request.WorkerEpoch, DesiredVersion: request.DesiredVersion, AbortDesiredVersion: cp.AbortDesiredVersion.Int64, CheckpointID: request.CheckpointID, WriterGeneration: cp.WriterGeneration, MembershipRevision: cp.MembershipRevision, VMPlatformID: i.VMPlatformID, Disposition: workerapi.CaptureAborted, WriteCapability: plan.WriteCapability, Members: []workerapi.CaptureAbortMember{}}
	if plan.Adopted {
		response.Disposition = workerapi.CaptureAdopted
	} else if plan.Acknowledged {
		response.Disposition = workerapi.CaptureAbortAcknowledged
	}
	for _, m := range plan.Members {
		response.Members = append(response.Members, workerapi.CaptureAbortMember{RunID: m.RunID, AttemptNumber: m.AttemptNumber, RunWaitID: m.RunWaitID, Lease: workerapi.RunLeaseFence{ID: m.LeaseID, LeaseSequence: m.LeaseSequence}, BaseComputerDiskVersionID: m.BaseComputerDiskVersionID, ExpiresAt: m.ExpiresAt, Cancelled: m.Cancelled})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerCompleteCaptureAbort(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CaptureAbortCompleteRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	ref, err := checkpointRef(workerFromContext(r.Context()), request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion, request.CheckpointID)
	if err != nil {
		writeError(w, err)
		return
	}
	cancelled := make([]uuid.UUID, 0, len(request.CancelledRunLeaseIDs))
	for _, value := range request.CancelledRunLeaseIDs {
		id, parseErr := uuid.Parse(value)
		if parseErr != nil {
			writeError(w, badRequest(parseErr))
			return
		}
		cancelled = append(cancelled, id)
	}
	if _, err = computer.CompleteCaptureAbort(r.Context(), s.tx, ref, request.AbortDesiredVersion, cancelled); err != nil {
		s.writeWorkerComputerError(w, err, computerCaptureAbortOperation, "acknowledge Computer capture abort")
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerCheckpointResponse{ComputerInstanceID: request.ComputerInstanceID, WorkerEpoch: request.WorkerEpoch, DesiredVersion: request.AbortDesiredVersion, CheckpointID: request.CheckpointID})
}
