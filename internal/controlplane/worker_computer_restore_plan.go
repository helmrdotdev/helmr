package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerComputerRestorePlan(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerRestorePlanRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	environmentID, err := parseCanonicalUUID("environment_id", request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	instanceID, err := parseCanonicalUUID("computer_instance_id", request.ComputerInstanceID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.WriterGeneration <= 0 {
		writeError(w, badRequest(errors.New("positive writer generation is required")))
		return
	}
	plan, err := computer.ReadRestorePlan(r.Context(), s.tx, s.computerFencingKey, workerFromContext(r.Context()), computer.WriterRef{EnvironmentID: environmentID, InstanceID: instanceID, WriterGeneration: request.WriterGeneration})
	if err != nil {
		mapped := computerError(err, computerRestorePlanOperation)
		if errorStatus(mapped) == http.StatusInternalServerError {
			s.log.Error("load Computer restore plan", "error", err)
		}
		writeError(w, mapped)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerRestorePlanResponse{Plan: workerRestorePlan(plan)})
}

func workerRestorePlan(plan *computer.RestorePlan) *workerapi.ComputerRestorePlan {
	if plan == nil {
		return nil
	}
	i := plan.Instance
	projected := &workerapi.ComputerRestorePlan{ComputerInstanceID: pgvalue.UUIDString(i.ID), ComputerID: pgvalue.UUIDString(i.ComputerID), CheckpointID: pgvalue.UUIDString(i.SourceCheckpointID), DesiredVersion: i.DesiredVersion, WriterGeneration: i.WriterGeneration, WorkerHostID: pgvalue.UUIDString(i.WorkerHostID), WorkerEpoch: i.WorkerEpoch, VMPlatformID: i.VMPlatformID, WriteCapability: plan.WriteCapability, Members: make([]workerapi.ComputerRestoreMember, 0, len(plan.Members))}
	for _, member := range plan.Members {
		projected.Members = append(projected.Members, workerapi.ComputerRestoreMember{RunID: member.RunID, AttemptNumber: member.AttemptNumber, Lease: workerapi.RunLeaseFence{ID: member.LeaseID, LeaseSequence: member.LeaseSequence}, BaseComputerDiskVersionID: member.BaseComputerDiskVersionID, ExpiresAt: member.ExpiresAt})
	}
	return projected
}
