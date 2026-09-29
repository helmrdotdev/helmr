package controlplane

import (
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerClaimComputerInstance(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	worker := workerFromContext(r.Context())
	targets, err := s.db.ListUnclaimedWorkerComputerInstances(r.Context(), db.ListUnclaimedWorkerComputerInstancesParams{WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch})
	if err != nil {
		writeError(w, errors.New("list prepared Computer Instances"))
		return
	}
	for _, target := range targets {
		token, err := auth.GenerateOpaque(32)
		if err != nil {
			writeError(w, errors.New("generate Computer Instance channel"))
			return
		}
		var assignment *workerapi.ComputerInstanceAssignment
		err = s.inTx(r.Context(), func(work *txWork) error {
			i, err := claimComputerInstanceChannel(r.Context(), work.tx, worker, target.ID, target.EnvironmentID, token)
			if err != nil {
				return err
			}
			source, err := work.q.GetComputerInstanceAssignmentSource(r.Context(), i.ID)
			if err != nil {
				return err
			}
			assignment = &workerapi.ComputerInstanceAssignment{
				ComputerInstanceID: pgvalue.UUIDString(i.ID), ComputerID: pgvalue.UUIDString(i.ComputerID), ComputerSpecID: pgvalue.UUIDString(i.ComputerSpecID),
				OrgID: pgvalue.UUIDString(i.OrgID), ProjectID: pgvalue.UUIDString(i.ProjectID), EnvironmentID: pgvalue.UUIDString(i.EnvironmentID),
				WriterGeneration: i.WriterGeneration, DesiredVersion: i.DesiredVersion, ObservedVersion: i.ObservedVersion, RuntimeEpoch: i.WorkerEpoch,
				Target:              workerapi.ComputerMountTarget{BaseComputerDiskVersionID: pgvalue.UUIDString(i.SourceDiskVersionID)},
				RestoreCheckpointID: pgvalue.UUIDString(i.SourceCheckpointID),
				GuestdChannelToken:  token, GuestdChannelTokenHash: hex.EncodeToString(i.GuestChannelTokenHash), ExpiresAt: i.WriterExpiresAt.Time,
				VMPlatformID: i.VMPlatformID, RootfsDigest: source.RootfsDigest, VMRuntimeContract: source.Contract,
				ComputerImage:     workerapi.CASObject{Digest: source.SeedDigest, SizeBytes: source.SeedSizeBytes, MediaType: source.SeedMediaType},
				ComputerMountPath: "/workspace", RequestedMilliCPU: i.ReservedCPUMillis, RequestedMemoryMiB: i.ReservedMemoryBytes / 1048576,
				RequestedDiskMiB: i.ReservedGuestEphemeralDiskBytes / 1048576, RequestedExecutionSlots: i.ReservedExecutionSlots,
			}
			return nil
		})
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			writeError(w, errors.New("claim Computer Instance channel"))
			return
		}
		writeJSON(w, http.StatusOK, workerapi.ComputerInstanceClaimResponse{Assignment: assignment})
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerInstanceClaimResponse{})
}
