package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (s *Server) workerRegisterCheckpointComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerCheckpointComputerObject(w, r, "register")
}
func (s *Server) workerCertifyCheckpointComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerCheckpointComputerObject(w, r, "certify")
}
func (s *Server) workerReuseCheckpointComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerCheckpointComputerObject(w, r, "reuse")
}
func (s *Server) workerCheckpointComputerObject(w http.ResponseWriter, r *http.Request, operation string) {
	var request workerapi.CheckpointComputerObjectRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
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
	object, err := describeComputerObject(request.Inspection)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	var uploaded *cas.Object
	if operation == "certify" {
		if err = s.recordCheckpointComputerObject(r.Context(), worker, request, nil, "verify"); err != nil {
			s.writeComputerPublicationError(w, err)
			return
		}
		stored, e := s.cas.Stat(r.Context(), object.digest)
		if e != nil {
			writeError(w, unavailable(errors.New("computer object unavailable")))
			return
		}
		uploaded = &stored
	}
	if err = s.recordCheckpointComputerObject(r.Context(), worker, request, uploaded, operation); err != nil {
		s.writeComputerPublicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
func (s *Server) recordCheckpointComputerObject(ctx context.Context, worker workergroup.HostPrincipal, request workerapi.CheckpointComputerObjectRequest, uploaded *cas.Object, operation string) error {
	return s.inTx(ctx, func(work *txWork) error {
		tx := work.tx
		q := db.New(tx)
		actor := dispatch.ComputerCaptureWorker{GroupID: pgvalue.UUID(worker.GroupID), HostID: pgvalue.UUID(worker.HostID), Epoch: worker.Epoch}
		instance, checkpoint, err := dispatch.LockComputerCheckpointPublication(ctx, tx, actor, request)
		if err != nil {
			return err
		}
		publicationKey := computerPublicationKey("checkpoint", checkpoint.ID, checkpoint.ID)
		keys, err := q.ListInstanceComputerSourceKeys(ctx, instance.ID)
		if err != nil {
			return err
		}
		allowed := make(map[string]bool, len(keys)+1)
		for _, key := range keys {
			allowed[pgvalue.UUIDString(key.ID)] = true
		}
		write, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: instance.ID, EnvironmentID: instance.EnvironmentID, ComputerID: instance.ComputerID})
		if err != nil {
			return err
		}
		if !instance.WriteKeyID.Valid || instance.WriteKeyID != write.ID {
			return conflict(errors.New("runtime write key is not pinned"))
		}
		allowed[pgvalue.UUIDString(write.ID)] = true
		owner := dispatch.ComputerPreparation{OrgID: instance.OrgID, ProjectID: instance.ProjectID, EnvironmentID: instance.EnvironmentID, ComputerID: instance.ComputerID, LogicalBytes: instance.ReservedGuestEphemeralDiskBytes}
		if operation == "verify" {
			object, err := describeComputerObject(request.Inspection)
			if err != nil {
				return err
			}
			row, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: owner.EnvironmentID, ComputerID: owner.ComputerID, Digest: object.digest})
			if err != nil {
				return err
			}
			var stored blockformat.ObjectInspection
			if err := json.Unmarshal(row.Inspection, &stored); err != nil {
				return err
			}
			if !reflect.DeepEqual(stored, request.Inspection) {
				return computerObjectConflict("object differs from registered inspection")
			}
			if _, err := q.RequireComputerObjectPin(ctx, db.RequireComputerObjectPinParams{ComputerInstanceID: instance.ID, PublicationKey: publicationKey, InstanceDesiredVersion: instance.DesiredVersion, Digest: object.digest}); err != nil {
				return err
			}
		} else if err = recordComputerObjectLocked(ctx, tx, owner, instance.ID, publicationKey, instance.DesiredVersion, request.Inspection, uploaded, operation == "reuse", allowed); err != nil {
			return err
		}
		if _, _, err = dispatch.LockComputerCheckpointPublication(ctx, tx, actor, request); err != nil {
			return err
		}
		return nil
	})
}
