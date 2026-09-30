package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerAcknowledgeComputerRestore(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerRestoreAckRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	instance, err := parseCanonicalUUID("computer_instance_id", request.ComputerInstanceID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	checkpoint, err := parseCanonicalUUID("checkpoint_id", request.CheckpointID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.DesiredVersion <= 0 || request.WriterGeneration <= 0 || request.Grants == nil {
		writeError(w, badRequest(errors.New("restore versions and explicit grants are required")))
		return
	}
	grants := make([]dispatch.ComputerRestoreGrant, 0, len(request.Grants))
	for _, g := range request.Grants {
		run, err := parseCanonicalUUID("run_id", g.RunID)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		lease, err := parseCanonicalUUID("lease.id", g.Lease.ID)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		if g.Lease.LeaseSequence <= 0 {
			writeError(w, badRequest(errors.New("lease sequence must be positive")))
			return
		}
		grants = append(grants, dispatch.ComputerRestoreGrant{RunID: pgvalue.UUID(run), LeaseID: pgvalue.UUID(lease), LeaseSequence: g.Lease.LeaseSequence})
	}
	worker := workerFromContext(r.Context())
	err = s.inTx(r.Context(), func(work *txWork) error {
		_, err := dispatch.AcknowledgeComputerRestore(r.Context(), work.tx, computer.InstanceRef{Host: computer.Host{GroupID: worker.GroupID, HostID: worker.HostID, Epoch: worker.Epoch}, ID: instance, DesiredVersion: request.DesiredVersion}, pgvalue.UUID(checkpoint), request.WriterGeneration, grants)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("Computer restore authority changed")))
		return
	}
	if err != nil {
		s.log.Error("Computer restore acknowledgement failed", "error", err)
		writeError(w, errors.New("acknowledge Computer restore"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerRestoreAckResponse{ComputerInstanceID: request.ComputerInstanceID, CheckpointID: request.CheckpointID, DesiredVersion: request.DesiredVersion, WriterGeneration: request.WriterGeneration})
}
