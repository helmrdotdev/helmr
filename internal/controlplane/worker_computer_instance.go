package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const workerInstanceReconcileLimit int32 = 64

func (s *Server) workerNextInstanceReconcileTarget(w http.ResponseWriter, r *http.Request) {
	var request workerapi.InstanceReconcileRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid runtime reconcile request JSON: %w", err))
		return
	}
	worker := workerFromContext(r.Context())
	targets, err := computer.ReconcileTargets(r.Context(), s.db, computer.Host{GroupID: worker.GroupID, HostID: worker.HostID, Epoch: worker.Epoch}, workerInstanceReconcileLimit)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]workerapi.InstanceReconcileTarget, 0, len(targets))
	for _, target := range targets {
		item, err := instanceReconcileTarget(r.Context(), s.platformStore, target)
		if err != nil {
			writeError(w, err)
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, workerapi.InstanceReconcileResponse{Items: items})
}

var instanceReconcileActions = map[computer.ReconcileAction]string{
	computer.ReconcilePrepare: workerapi.InstanceReconcilePrepare,
	computer.ReconcileCapture: workerapi.InstanceReconcileCapture,
	computer.ReconcileClose:   workerapi.InstanceReconcileClose,
	computer.ReconcileReclaim: workerapi.InstanceReconcileReclaim,
}

// runtimeReconcileTarget projects a reconcile target onto the worker
// contract: its action, the capture it takes, and for preparation the
// Computer and Program sources and the checkpoint it restores.
func instanceReconcileTarget(ctx context.Context, platform cas.Reader, target computer.ReconcileTarget) (workerapi.InstanceReconcileTarget, error) {
	row := target.Instance
	var capture *workerapi.InstanceCapture
	if target.Capture != nil {
		var err error
		if capture, err = projectComputerInstanceCapture(target.Capture.Checkpoint, target.Capture.Members); err != nil {
			return workerapi.InstanceReconcileTarget{}, err
		}
	}
	source := computerInstanceSourceMetadata(row)
	if target.Action == computer.ReconcilePrepare {
		var err error
		if source, err = projectComputerInstancePreparation(ctx, platform, row); err != nil {
			return workerapi.InstanceReconcileTarget{}, err
		}
		if target.Restore != nil {
			restore, err := projectComputerInstanceRestore(target.Restore.Checkpoint, target.Restore.Members)
			if err != nil {
				return workerapi.InstanceReconcileTarget{}, err
			}
			source.Restore = &restore
		}
	}
	return workerapi.InstanceReconcileTarget{
		ID: pgvalue.UUIDString(row.ID), WorkerEpoch: row.WorkerEpoch,
		DesiredVersion: row.DesiredVersion, ObservedVersion: row.ObservedVersion,
		Action: instanceReconcileActions[target.Action], Source: source, Capture: capture, PreparationExpiresAt: row.PreparationExpiresAt.Time,
	}, nil
}

func (s *Server) workerMarkComputerInstanceReady(w http.ResponseWriter, r *http.Request) {
	request, observation, ok := decodeComputerInstanceObservation(w, r, "ready")
	if !ok {
		return
	}
	if request.VMVCPUCount <= 0 {
		writeError(w, badRequest(errors.New("vm_vcpu_count must be positive")))
		return
	}
	if !sha256sum.ValidDigest(request.CPUConfigDigest) {
		writeError(w, badRequest(errors.New("cpu_config_digest must be a canonical SHA-256 digest")))
		return
	}
	row, err := computer.RecordInstanceReady(r.Context(), s.tx, computer.Readiness{Observation: observation, VCPUCount: request.VMVCPUCount, CPUConfigDigest: request.CPUConfigDigest})
	s.writeComputerInstanceObservation(w, row, err)
}

func (s *Server) workerMarkComputerInstanceClosed(w http.ResponseWriter, r *http.Request) {
	request, observation, ok := decodeComputerInstanceObservation(w, r, "closed")
	if !ok {
		return
	}
	reason := strings.TrimSpace(request.ReasonCode)
	if reason == "" {
		reason = "desired_state_reconciled"
	}
	row, err := computer.RecordInstanceClosed(r.Context(), s.tx, computer.Closure{Observation: observation, Reason: reason, CleanupProof: computerCleanupProof(request.CleanupProof)})
	s.writeComputerInstanceObservation(w, row, err)
}

func (s *Server) workerMarkComputerInstanceFailed(w http.ResponseWriter, r *http.Request) {
	request, observation, ok := decodeComputerInstanceObservation(w, r, "failed")
	if !ok {
		return
	}
	reason := strings.TrimSpace(request.ReasonCode)
	if reason == "" {
		reason = "runtime_reconcile_failed"
	}
	kind := computer.FailureInstance
	switch reason {
	case workerapi.InstanceFailureWorkerInvalid:
		kind = computer.FailureWorkerInvalid
	case workerapi.InstanceFailureComputerSource:
		kind = computer.FailureSourceUnavailable
	}
	row, err := computer.RecordInstanceFailure(r.Context(), s.tx, computer.Failure{
		Observation: observation, Kind: kind, Reason: reason, Error: normalizedJSONRawMessage(request.Error),
		CleanupProof: computerCleanupProof(request.CleanupProof),
	})
	s.writeComputerInstanceObservation(w, row, err)
}

// decodeComputerInstanceObservation decodes an Instance observation and
// checks its fences against the authenticated host epoch.
func decodeComputerInstanceObservation(w http.ResponseWriter, r *http.Request, state string) (workerapi.ComputerInstanceStateRequest, computer.Observation, bool) {
	var request workerapi.ComputerInstanceStateRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker runtime instance %s request JSON: %w", state, err))
		return request, computer.Observation{}, false
	}
	id, err := ids.Parse(request.ID)
	if err != nil {
		writeError(w, badRequest(errors.New("id must be a canonical UUIDv7")))
		return request, computer.Observation{}, false
	}
	if request.WorkerEpoch <= 0 || request.DesiredVersion <= 0 || request.ExpectedObservedVersion < 0 {
		writeError(w, badRequest(errors.New("runtime epoch, desired version, and observed version fences are required")))
		return request, computer.Observation{}, false
	}
	worker := workerFromContext(r.Context())
	if request.WorkerEpoch != worker.Epoch {
		writeError(w, forbidden(errors.New("runtime instance belongs to another worker epoch")))
		return request, computer.Observation{}, false
	}
	return request, computer.Observation{
		Instance: computer.InstanceRef{
			Host: computer.Host{GroupID: worker.GroupID, HostID: worker.HostID, Epoch: worker.Epoch},
			ID:   id, DesiredVersion: request.DesiredVersion,
		},
		ExpectedObservedVersion: request.ExpectedObservedVersion,
	}, true
}

func computerCleanupProof(proof *workerapi.InstanceCleanupProof) *computer.CleanupProof {
	if proof == nil {
		return nil
	}
	return &computer.CleanupProof{Method: proof.Method, CompletedAt: proof.CompletedAt}
}

func (s *Server) writeComputerInstanceObservation(w http.ResponseWriter, row db.ComputerInstance, err error) {
	if err != nil {
		s.writeWorkerComputerError(w, err, computerInstanceObservationOperation, "Computer Instance observation failed")
		return
	}
	writeJSON(w, http.StatusOK, computerInstanceResponse(row))
}

func normalizedJSONRawMessage(raw json.RawMessage) []byte {
	if strings.TrimSpace(string(raw)) == "" {
		return []byte(`{}`)
	}
	return []byte(raw)
}

func computerInstanceResponse(row db.ComputerInstance) workerapi.ComputerInstance {
	return workerapi.ComputerInstance{
		ID:                     pgvalue.UUIDString(row.ID),
		OrgID:                  pgvalue.UUIDString(row.OrgID),
		ProjectID:              pgvalue.UUIDString(row.ProjectID),
		EnvironmentID:          pgvalue.UUIDString(row.EnvironmentID),
		WorkerHostID:           pgvalue.UUIDString(row.WorkerHostID),
		WorkerEpoch:            row.WorkerEpoch,
		RuntimeID:              row.VMPlatformID,
		VMVCPUCount:            row.VMVCPUCount,
		CPUConfigDigest:        row.CPUConfigDigest,
		ComputerSpecID:         pgvalue.UUIDString(row.ComputerSpecID),
		Status:                 string(row.ObservedState),
		ReservedCPUMillis:      int32(row.ReservedCPUMillis),
		ReservedMemoryMiB:      int32(row.ReservedMemoryBytes / 1048576),
		ReservedDiskMiB:        row.ReservedGuestEphemeralDiskBytes / 1048576,
		ReservedExecutionSlots: row.ReservedExecutionSlots,
	}
}
