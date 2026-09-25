package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"net/http"
	"reflect"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerRegisterRunComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerRunComputerObject(w, r, "register")
}
func (s *Server) workerCertifyRunComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerRunComputerObject(w, r, "certify")
}
func (s *Server) workerReuseRunComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerRunComputerObject(w, r, "reuse")
}
func (s *Server) workerRunComputerObject(w http.ResponseWriter, r *http.Request, operation string) {
	var request workerapi.RunComputerObjectRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
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
		// Ensure a live, exact registration before remote I/O. The final transaction
		// rechecks the same authority after storage responds.
		if err = s.recordRunComputerObject(r.Context(), worker, request, nil, "verify"); err != nil {
			s.writeRunComputerObjectError(w, err)
			return
		}
		if s.cas == nil {
			writeError(w, unavailable(errors.New("computer storage unavailable")))
			return
		}
		stored, e := s.cas.Stat(r.Context(), object.digest)
		if e != nil {
			writeError(w, unavailable(errors.New("computer object unavailable")))
			return
		}
		uploaded = &stored
	}
	if err = s.recordRunComputerObject(r.Context(), worker, request, uploaded, operation); err != nil {
		s.writeRunComputerObjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) recordRunComputerObject(ctx context.Context, worker workerActor, request workerapi.RunComputerObjectRequest, uploaded *cas.Object, operation string) error {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return err
	}
	if (request.Checkpoint == nil) == (request.OperationID == "") {
		return computerObjectConflict("one publication owner required")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)
	var authority runLeaseClaimAuthority
	var expires pgtype.Timestamptz
	var publicationKey []byte
	if cp := request.Checkpoint; cp != nil {
		id, err := parseCanonicalUUID("checkpoint_id", cp.ID)
		publicationKey = computerPublicationKey("checkpoint", pgvalue.UUID(id), pgvalue.UUID(id))
		if err != nil {
			return err
		}
		wait, err := parseCanonicalUUID("run_wait_id", cp.RunWaitID)
		if err != nil {
			return err
		}
		if cp.RequestVersion <= 0 {
			return computerObjectConflict("checkpoint request version required")
		}
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT candidate_manifest FROM run_checkpoints WHERE id=$1 AND status='creating'`, pgvalue.UUID(id)).Scan(&raw); err != nil {
			return err
		}
		var manifest workerapi.CheckpointManifest
		if err = json.Unmarshal(raw, &manifest); err != nil {
			return err
		}
		source, err := lockCheckpointSource(ctx, &txWork{q: q, tx: tx}, worker, lease, request.Lease.LeaseSequence, wait, id, cp.RequestVersion, manifest)
		if err != nil {
			return err
		}
		authority, expires = source.authority, source.expiresAt
		if _, err = q.RequireRegisteredCheckpointManifest(ctx, db.RequireRegisteredCheckpointManifestParams{ID: pgvalue.UUID(id), Manifest: raw}); err != nil {
			return err
		}
	} else {
		operation, err := parseCanonicalUUID("operation_id", request.OperationID)
		publicationKey = computerPublicationKey("finalization", pgvalue.UUID(lease.leaseID), pgvalue.UUID(operation))
		if err != nil {
			return err
		}
		locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: pgvalue.UUID(lease.leaseID), LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch})
		if err != nil {
			return err
		}
		if _, err = secret.LockAttemptDelivery(ctx, q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID); err != nil {
			return err
		}
		authority, err = lockRunPublicationAuthority(ctx, q, worker, pgvalue.UUID(lease.leaseID), request.Lease.LeaseSequence, locators, db.RunStatusRunning)
		if err != nil {
			return err
		}
		if err = validateRunFinalizationOwner(authority, locators); err != nil {
			return err
		}
		if err = lockSameWorkspaceChildFinalization(ctx, q, &authority); err != nil {
			return err
		}
		if authority.runLease.Status != db.RunLeaseStatusFinalizing || authority.runLease.FinalizationOperationID != pgvalue.UUID(operation) {
			return errStaleRunFinalization
		}
		var registered bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_finalization_objects WHERE run_lease_id=$1 AND operation_id=$2 AND lease_status='finalizing')`, authority.runLease.ID, pgvalue.UUID(operation)).Scan(&registered); err != nil {
			return err
		}
		if !registered {
			return computerObjectConflict("finalization candidate is not registered")
		}
	}
	keys, err := q.ListRuntimeComputerSourceKeys(ctx, authority.runtime.ID)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(keys)+1)
	for _, key := range keys {
		allowed[pgvalue.UUIDString(key.ID)] = true
	}
	write, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{RuntimeInstanceID: authority.runtime.ID, EnvironmentID: authority.run.EnvironmentID, ComputerID: authority.workspace.ID})
	if err != nil {
		return err
	}
	if !authority.runtime.ComputerWriteKeyID.Valid || authority.runtime.ComputerWriteKeyID != write.ID {
		return conflict(errors.New("Runtime write key is not pinned"))
	}
	allowed[pgvalue.UUIDString(write.ID)] = true
	owner := dispatch.ComputerPreparation{OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID, ComputerID: authority.workspace.ID, LogicalBytes: authority.runtime.ReservedGuestEphemeralDiskBytes}
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
		if _, err := q.RequireRuntimeComputerObjectPin(ctx, db.RequireRuntimeComputerObjectPinParams{RuntimeInstanceID: authority.runtime.ID, PublicationKey: publicationKey, RuntimeDesiredVersion: authority.runtime.DesiredVersion, Digest: object.digest}); err != nil {
			return err
		}
	} else if err = recordComputerObjectLocked(ctx, tx, owner, authority.runtime.ID, publicationKey, authority.runtime.DesiredVersion, request.Inspection, uploaded, operation == "reuse", allowed); err != nil {
		return err
	}
	now, err := q.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return err
	}
	if !now.Valid || !now.Time.Before(authority.runLease.ExpiresAt.Time) || !now.Time.Before(authority.workspaceLease.ExpiresAt.Time) || (expires.Valid && !now.Time.Before(expires.Time)) {
		return errStaleRunLeaseClaim
	}
	return tx.Commit(ctx)
}

func (s *Server) writeRunComputerObjectError(w http.ResponseWriter, err error) {
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, errStaleRunFinalization) || isDeterministicWorkerAdmission(err) {
		writeError(w, conflict(errors.New("Computer publication authority changed")))
		return
	}
	if errorStatus(err) < 500 {
		writeError(w, err)
		return
	}
	s.log.Error("Computer object publication failed", "error", err)
	writeError(w, errors.New("Computer object publication failed"))
}
