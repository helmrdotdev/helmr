package controlplane

import (
	"encoding/hex"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerClaimComputerInstance(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	claimed, err := computer.ClaimInstance(r.Context(), s.db, s.tx, workerFromContext(r.Context()))
	if err != nil {
		s.writeWorkerComputerError(w, err, computerInstanceClaimOperation, "Computer Instance claim failed")
		return
	}
	if claimed == nil {
		writeJSON(w, http.StatusOK, workerapi.ComputerInstanceClaimResponse{})
		return
	}
	i, source := claimed.Instance, claimed.Source
	writeJSON(w, http.StatusOK, workerapi.ComputerInstanceClaimResponse{Assignment: &workerapi.ComputerInstanceAssignment{
		ComputerInstanceID: pgvalue.UUIDString(i.ID), ComputerID: pgvalue.UUIDString(i.ComputerID), ComputerSpecID: pgvalue.UUIDString(i.ComputerSpecID),
		OrgID: pgvalue.UUIDString(i.OrgID), ProjectID: pgvalue.UUIDString(i.ProjectID), EnvironmentID: pgvalue.UUIDString(i.EnvironmentID),
		WriterGeneration: i.WriterGeneration, DesiredVersion: i.DesiredVersion, ObservedVersion: i.ObservedVersion, WorkerEpoch: i.WorkerEpoch,
		Target:                 workerapi.ComputerMountTarget{BaseComputerDiskVersionID: pgvalue.UUIDString(i.SourceDiskVersionID)},
		RestoreCheckpointID:    pgvalue.UUIDString(i.SourceCheckpointID),
		GuestChannelCredential: claimed.ChannelCredential, GuestChannelCredentialHash: hex.EncodeToString(i.GuestChannelCredentialHash), ExpiresAt: i.WriterExpiresAt.Time,
		VMPlatformID: i.VMPlatformID, RootfsDigest: source.RootfsDigest, VMRuntimeContract: source.Contract,
		ComputerImage:     workerapi.CASObject{Digest: source.SeedDigest, SizeBytes: source.SeedSizeBytes, MediaType: source.SeedMediaType},
		ComputerMountPath: "/workspace", RequestedMilliCPU: i.ReservedCPUMillis, RequestedMemoryMiB: i.ReservedMemoryBytes / 1048576,
		RequestedDiskMiB: i.ReservedGuestEphemeralDiskBytes / 1048576, RequestedExecutionSlots: i.ReservedExecutionSlots,
	}})
}
