package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (s *Server) workerRegisterCheckpoint(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RegisterCheckpointRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	ref, err := checkpointRef(workerFromContext(r.Context()), request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion, request.CheckpointID)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err = computer.RegisterCheckpoint(r.Context(), s.tx, ref, computerCheckpointManifest(request.Manifest)); err != nil {
		s.writeWorkerComputerError(w, err, computerCheckpointRegisterOperation, "computer object publication failed")
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerCheckpointResponse{ComputerInstanceID: request.ComputerInstanceID, WorkerEpoch: request.WorkerEpoch, DesiredVersion: request.DesiredVersion, CheckpointID: request.CheckpointID})
}

func (s *Server) workerMarkCheckpointReady(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CheckpointReadyRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	ref, err := checkpointRef(workerFromContext(r.Context()), request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion, request.CheckpointID)
	if err != nil {
		writeError(w, err)
		return
	}
	checkpoint, err := s.publisher.CompleteCheckpoint(r.Context(), ref, computerCheckpointManifest(request.Manifest))
	if err != nil {
		s.writeWorkerComputerError(w, err, computerCheckpointReadyOperation, "computer object publication failed")
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerCheckpointResponse{ComputerInstanceID: request.ComputerInstanceID, WorkerEpoch: request.WorkerEpoch, DesiredVersion: request.DesiredVersion, CheckpointID: request.CheckpointID, ComputerDiskVersionID: pgvalue.UUIDString(checkpoint.PrivateComputerDiskVersionID)})
}

// checkpointRef addresses the capture checkpoint a worker request names on
// the authenticated host epoch. Malformed identifiers and non-positive
// versions are rejected before any database access.
func checkpointRef(worker workergroup.HostPrincipal, instance string, workerEpoch, desiredVersion int64, checkpoint string) (computer.CheckpointRef, error) {
	instanceID, err := parseCanonicalUUID("computer_instance_id", instance)
	if err != nil {
		return computer.CheckpointRef{}, badRequest(err)
	}
	checkpointID, err := parseCanonicalUUID("checkpoint_id", checkpoint)
	if err != nil {
		return computer.CheckpointRef{}, badRequest(err)
	}
	if workerEpoch <= 0 || desiredVersion <= 0 {
		return computer.CheckpointRef{}, badRequest(errors.New("checkpoint source versions must be positive"))
	}
	return computer.CheckpointRef{
		Host:       computer.Host{GroupID: worker.GroupID, HostID: worker.HostID, Epoch: worker.Epoch},
		InstanceID: instanceID, WorkerEpoch: workerEpoch, DesiredVersion: desiredVersion, CheckpointID: checkpointID,
	}, nil
}

// computerCheckpointManifest is the Computer owner's form of a worker's
// checkpoint manifest. Both types encode to the same JSON, which the owner
// persists and fingerprints.
func computerCheckpointManifest(m workerapi.CheckpointManifest) computer.CheckpointManifest {
	point := m.RecoveryPoint
	runtime := point.Runtime
	result := computer.CheckpointManifest{
		RecoveryPoint: computer.CheckpointRecoveryPoint{
			ID: point.ID, ComputerID: point.ComputerID, ComputerInstanceID: point.ComputerInstanceID,
			WriterGeneration: point.WriterGeneration, MembershipRevision: point.MembershipRevision,
			ComputerSpecID: point.ComputerSpecID, ProgramDeploymentID: point.ProgramDeploymentID,
			Runtime: computer.CheckpointRuntime{
				Backend: runtime.Backend, ID: runtime.ID, Arch: runtime.Arch, Contract: runtime.Contract,
				KernelDigest: runtime.KernelDigest, InitramfsDigest: runtime.InitramfsDigest, RootfsDigest: runtime.RootfsDigest,
				ConfigDigest: runtime.ConfigDigest, VMVCPUCount: runtime.VMVCPUCount, CPUConfigDigest: runtime.CPUConfigDigest,
			},
		},
		RuntimeState: computer.CheckpointRuntimeState{
			ConfigArtifact:      computerCheckpointArtifact(m.RuntimeState.ConfigArtifact),
			VMStateArtifact:     computerCheckpointArtifact(m.RuntimeState.VMStateArtifact),
			ScratchDiskArtifact: computerCheckpointArtifact(m.RuntimeState.ScratchDiskArtifact),
			Config:              m.RuntimeState.Config,
		},
		ComputerState: computer.CheckpointComputerState{Base: computer.CheckpointComputerBase{MountPath: m.ComputerState.Base.MountPath}},
	}
	// A nil member list is a rejected candidate, distinct from an empty one.
	if point.Runs != nil {
		result.RecoveryPoint.Runs = make([]computer.CheckpointRun, 0, len(point.Runs))
		for _, run := range point.Runs {
			result.RecoveryPoint.Runs = append(result.RecoveryPoint.Runs, computer.CheckpointRun{
				RunID: run.RunID, AttemptNumber: run.AttemptNumber, RunWaitID: run.RunWaitID, RunLeaseID: run.RunLeaseID,
				ActorSpeculativeInputSequence: run.ActorSpeculativeInputSequence, CorrelationID: run.CorrelationID,
			})
		}
	}
	if captured := m.RuntimeState.Computer; captured != nil {
		result.RuntimeState.Computer = &computer.CheckpointComputer{ComputerID: captured.ComputerID, LogicalBytes: captured.LogicalBytes, Root: captured.Root}
	}
	for _, artifact := range m.RuntimeState.MemoryArtifacts {
		result.RuntimeState.MemoryArtifacts = append(result.RuntimeState.MemoryArtifacts, computerCheckpointArtifact(artifact))
	}
	// Absent timings persist as no timings, distinct from an empty list.
	if m.Phases != nil {
		result.Phases = make([]computer.CheckpointPhase, 0, len(m.Phases))
	}
	for _, phase := range m.Phases {
		converted := computer.CheckpointPhase{Name: phase.Name, DurationMs: phase.DurationMs, Role: phase.Role, MediaType: phase.MediaType, ErrorClass: phase.ErrorClass}
		if phase.Filepack != nil {
			converted.Filepack = &computer.CheckpointFilepackStats{LogicalBytes: phase.Filepack.LogicalBytes, EncodedChunks: phase.Filepack.EncodedChunks, UnpackWrittenBytes: phase.Filepack.UnpackWrittenBytes}
		}
		result.Phases = append(result.Phases, converted)
	}
	return result
}

func computerCheckpointArtifact(a workerapi.CheckpointArtifact) computer.CheckpointArtifact {
	return computer.CheckpointArtifact{Digest: a.Digest, SizeBytes: a.SizeBytes, MediaType: a.MediaType}
}
