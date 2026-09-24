package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

// Lock secrets, Computer, then the exact process/mount/Runtime writer. A process
// exit request owns publication until it is settled; no Run fence is fabricated.
func lockExecComputerPublication(ctx context.Context, tx pgx.Tx, q db.Querier, worker workerActor, org, mount string) (db.LockWorkspaceExecWorkerAuthorityRow, error) {
	var zero db.LockWorkspaceExecWorkerAuthorityRow
	orgID, mountID, err := parseWorkspaceWorkerIDs(org, mount)
	if err != nil {
		return zero, err
	}
	locator, err := q.GetWorkspaceExecLocatorForMount(ctx, db.GetWorkspaceExecLocatorForMountParams{OrgID: orgID, WorkspaceMountID: mountID})
	if err != nil {
		return zero, err
	}
	valid, err := lockWorkspaceExecPublicationSecrets(ctx, q, locator.ID, locator.WorkspaceID)
	if err != nil {
		return zero, err
	}
	if !valid {
		return zero, computerObjectConflict("exec secrets revoked")
	}
	if _, err = q.LockWorkspaceExecFailureWorkspace(ctx, db.LockWorkspaceExecFailureWorkspaceParams{OrgID: orgID, WorkspaceID: locator.WorkspaceID}); err != nil {
		return zero, err
	}
	a, err := q.LockWorkspaceExecWorkerAuthority(ctx, db.LockWorkspaceExecWorkerAuthorityParams{OrgID: orgID, ProcessID: locator.ID, WorkspaceMountID: mountID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch, ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds})
	if err != nil {
		return zero, err
	}
	if a.WorkspaceProcess.Status != db.WorkspaceProcessStatusExitRequested || a.WorkspaceMount.Status != "unmounting" || a.WorkspaceMount.FinalizationKind.String != "capture" || a.RuntimeInstance.ReclaimedAt.Valid {
		return zero, computerObjectConflict("exec is not publishing")
	}
	var claims bool
	if err = tx.QueryRow(ctx, `SELECT w.claim_version=$3 AND g.claim_version=$4 FROM worker_instances w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1 AND g.id=$2`, pgvalue.UUID(worker.WorkerInstanceID), pgvalue.UUID(worker.WorkerGroupID), worker.ClaimVersion, worker.GroupClaimVersion).Scan(&claims); err != nil {
		return zero, err
	}
	if !claims {
		return zero, computerObjectConflict("exec worker claims changed")
	}
	return a, nil
}
func checkExecPublicationDeadline(ctx context.Context, q db.Querier, a db.LockWorkspaceExecWorkerAuthorityRow) error {
	now, err := q.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return err
	}
	if !now.Valid || !a.WorkspaceLease.ExpiresAt.Valid || !now.Time.Before(a.WorkspaceLease.ExpiresAt.Time) {
		return computerObjectConflict("exec lease expired")
	}
	return nil
}
func (s *Server) workerRegisterExecComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerExecComputerObject(w, r, "register")
}
func (s *Server) workerCertifyExecComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerExecComputerObject(w, r, "certify")
}
func (s *Server) workerReuseExecComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerExecComputerObject(w, r, "reuse")
}
func (s *Server) workerExecComputerObject(w http.ResponseWriter, r *http.Request, operation string) {
	var request workerapi.ExecComputerObjectRequest
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
		if err = s.recordExecComputerObject(r.Context(), worker, request, nil, "verify"); err != nil {
			s.writeRunComputerObjectError(w, err)
			return
		}
		if s.cas == nil {
			writeError(w, unavailable(errors.New("Computer storage unavailable")))
			return
		}
		stored, e := s.cas.Stat(r.Context(), object.digest)
		if e != nil {
			writeError(w, unavailable(errors.New("Computer object unavailable")))
			return
		}
		uploaded = &stored
	}
	if err = s.recordExecComputerObject(r.Context(), worker, request, uploaded, operation); err != nil {
		s.writeRunComputerObjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
func (s *Server) recordExecComputerObject(ctx context.Context, worker workerActor, request workerapi.ExecComputerObjectRequest, uploaded *cas.Object, operation string) error {
	return s.withExecComputerPublication(ctx, worker, request.OrgID, request.WorkspaceMountID, func(tx pgx.Tx, q db.Querier, a db.LockWorkspaceExecWorkerAuthorityRow) error {
		runtime := a.RuntimeInstance
		process := a.WorkspaceProcess
		keys, err := q.ListRuntimeComputerSourceKeys(ctx, runtime.ID)
		if err != nil {
			return err
		}
		allowed := make(map[string]bool, len(keys)+1)
		for _, key := range keys {
			allowed[pgvalue.UUIDString(key.ID)] = true
		}
		write, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{RuntimeInstanceID: runtime.ID, EnvironmentID: process.EnvironmentID, ComputerID: process.WorkspaceID})
		if err != nil {
			return err
		}
		if !runtime.ComputerWriteKeyID.Valid || runtime.ComputerWriteKeyID != write.ID {
			return computerObjectConflict("Runtime write key is not pinned")
		}
		allowed[pgvalue.UUIDString(write.ID)] = true
		owner := dispatch.ComputerPreparation{OrgID: process.OrgID, ProjectID: process.ProjectID, EnvironmentID: process.EnvironmentID, ComputerID: process.WorkspaceID, LogicalBytes: runtime.ReservedGuestEphemeralDiskBytes}
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
			if err = json.Unmarshal(row.Inspection, &stored); err != nil {
				return err
			}
			if !reflect.DeepEqual(stored, request.Inspection) {
				return computerObjectConflict("object differs from registered inspection")
			}
			if _, err = q.RequireRuntimeComputerObjectPin(ctx, db.RequireRuntimeComputerObjectPinParams{RuntimeInstanceID: runtime.ID, PublicationKey: computerPublicationKey("exec", process.ID, process.ID), RuntimeDesiredVersion: runtime.DesiredVersion, Digest: object.digest}); err != nil {
				return err
			}
		} else if err = recordComputerObjectLocked(ctx, tx, owner, runtime.ID, computerPublicationKey("exec", process.ID, process.ID), runtime.DesiredVersion, request.Inspection, uploaded, operation == "reuse", allowed); err != nil {
			return err
		}
		return nil
	})
}

func (s *Server) withExecComputerPublication(ctx context.Context, worker workerActor, org, mount string, fn func(pgx.Tx, db.Querier, db.LockWorkspaceExecWorkerAuthorityRow) error) error {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)
	a, err := lockExecComputerPublication(ctx, tx, q, worker, org, mount)
	if err != nil {
		return err
	}
	if err = fn(tx, q, a); err != nil {
		return err
	}
	if err = checkExecPublicationDeadline(ctx, q, a); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
