package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type runtimeReconcileTargetStore struct {
	db.Querier
	rows   []db.ListComputerInstanceReconcileTargetsRow
	params db.ListComputerInstanceReconcileTargetsParams
}

func (s *runtimeReconcileTargetStore) ListComputerInstanceReconcileTargets(
	_ context.Context,
	params db.ListComputerInstanceReconcileTargetsParams,
) ([]db.ListComputerInstanceReconcileTargetsRow, error) {
	s.params = params
	return s.rows, nil
}

func TestWorkerRuntimeReconcileTargetRoundTripsActionComputerAuthority(t *testing.T) {
	runtimeID := pgvalue.UUID(uuid.NewV7())
	workerID := uuid.NewV7()
	baseComputerDiskVersionID := pgvalue.UUID(uuid.NewV7())
	tests := []struct {
		name       string
		desired    string
		observed   string
		wantAction string
		wantTarget bool
	}{
		{name: "prepare", desired: "ready", observed: "allocated", wantAction: workerapi.RuntimeReconcilePrepare, wantTarget: true},
		{name: "close", desired: "closed", observed: "ready", wantAction: workerapi.RuntimeReconcileClose},
		{name: "reclaim", desired: "ready", observed: "failed", wantAction: workerapi.RuntimeReconcileReclaim},
		{name: "lost", desired: "closed", observed: "lost", wantAction: workerapi.RuntimeReconcileReclaim},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			row := initializingComputerSourceRow(t)
			row.ID, row.WorkerEpoch = runtimeID, 7
			row.DesiredState, row.ObservedState = test.desired, test.observed
			row.PreparationDiskVersionID = baseComputerDiskVersionID
			server := &Server{log: discardTestLogger(), db: &runtimeReconcileTargetStore{rows: []db.ListComputerInstanceReconcileTargetsRow{row}}}
			request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/computer-instances/reconcile", strings.NewReader(`{}`))
			request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
				WorkerHostID:  workerID,
				WorkerGroupID: controlplaneTestWorkerGroupID,
				WorkerEpoch:   7,
			}))
			response := httptest.NewRecorder()

			server.workerNextRuntimeReconcileTarget(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", response.Code, response.Body)
			}
			var decoded workerapi.RuntimeReconcileResponse
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("decode response %s: %v", response.Body, err)
			}
			if len(decoded.Items) != 1 || decoded.Items[0].Action != test.wantAction ||
				(decoded.Items[0].Source.Computer != nil) != test.wantTarget {
				t.Fatalf("items = %#v", decoded.Items)
			}
		})
	}
}

func TestWorkerRuntimeReconcileReturnsBoundedBatch(t *testing.T) {
	workerID := uuid.NewV7()
	store := &runtimeReconcileTargetStore{rows: []db.ListComputerInstanceReconcileTargetsRow{
		{ID: pgvalue.UUID(uuid.NewV7()), WorkerEpoch: 7, DesiredState: "closed", ObservedState: "ready"},
		{ID: pgvalue.UUID(uuid.NewV7()), WorkerEpoch: 7, DesiredState: "closed", ObservedState: "allocated"},
	}}
	server := &Server{log: discardTestLogger(), db: store}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/computer-instances/reconcile", strings.NewReader(`{}`))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{
		WorkerHostID: workerID, WorkerGroupID: controlplaneTestWorkerGroupID, WorkerEpoch: 7,
	}))
	response := httptest.NewRecorder()

	server.workerNextRuntimeReconcileTarget(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	var decoded workerapi.RuntimeReconcileResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 2 || store.params.RowLimit != workerRuntimeReconcileLimit ||
		store.params.WorkerEpoch != 7 || store.params.WorkerGroupID != controlplaneTestWorkerGroupDBID {
		t.Fatalf("items = %d params = %+v", len(decoded.Items), store.params)
	}
}

func TestComputerInstanceResponsePreservesActualCPUShape(t *testing.T) {
	row := db.ComputerInstance{
		VMVCPUCount:     3,
		CPUConfigDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	response := computerInstanceResponse(row)
	if response.VMVCPUCount != row.VMVCPUCount || response.CPUConfigDigest != row.CPUConfigDigest {
		t.Fatalf("response CPU shape = %d/%q, want %d/%q", response.VMVCPUCount, response.CPUConfigDigest, row.VMVCPUCount, row.CPUConfigDigest)
	}
}

func TestValidateRuntimeCleanupProofIsTypedAndTimeBounded(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{
		workerapi.RuntimeCleanupSessionClosed,
		workerapi.RuntimeCleanupHostReconciled,
		workerapi.RuntimeCleanupNotMaterialized,
	} {
		if err := validateRuntimeCleanupProof(workerapi.RuntimeCleanupProof{Method: method, CompletedAt: now}, now); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	for _, proof := range []workerapi.RuntimeCleanupProof{
		{Method: "assumed", CompletedAt: now},
		{Method: workerapi.RuntimeCleanupHostReconciled},
		{Method: workerapi.RuntimeCleanupHostReconciled, CompletedAt: now.Add(2 * time.Minute)},
	} {
		if err := validateRuntimeCleanupProof(proof, now); err == nil {
			t.Fatalf("invalid proof accepted: %+v", proof)
		}
	}
}

func TestValidateRuntimeClosedCleanupProofRequiresPhysicalTeardown(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{
		workerapi.RuntimeCleanupSessionClosed,
		workerapi.RuntimeCleanupHostReconciled,
	} {
		if err := validateRuntimeClosedCleanupProof(workerapi.RuntimeCleanupProof{Method: method, CompletedAt: now}, now); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	if err := validateRuntimeClosedCleanupProof(workerapi.RuntimeCleanupProof{
		Method: workerapi.RuntimeCleanupNotMaterialized, CompletedAt: now,
	}, now); err == nil {
		t.Fatal("not_materialized proof released a closed runtime")
	}
}

func TestWorkerReconcileDoesNotReplayOpenedCheckpoint(t *testing.T) {
	row := committedComputerSourceRow(t)
	row.ID = pgvalue.UUID(uuid.NewV7())
	row.WorkerEpoch = 7
	row.DesiredState = "ready"
	row.ObservedState = "ready"
	row.AdmissionState = "open"
	row.DesiredVersion = 2
	row.ObservedDesiredVersion = 1
	row.SourceCheckpointID = pgvalue.UUID(uuid.NewV7())
	row.SourceDiskVersionID = row.PreparationDiskVersionID
	store := &runtimeReconcileTargetStore{rows: []db.ListComputerInstanceReconcileTargetsRow{row}}
	server := &Server{log: discardTestLogger(), db: store}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/computer-instances/reconcile", strings.NewReader(`{}`))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, workerActor{WorkerHostID: uuid.NewV7(), WorkerGroupID: controlplaneTestWorkerGroupID, WorkerEpoch: 7}))
	response := httptest.NewRecorder()
	server.workerNextRuntimeReconcileTarget(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	var decoded workerapi.RuntimeReconcileResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 1 || decoded.Items[0].Source.Restore != nil || decoded.Items[0].Source.Computer == nil {
		t.Fatalf("checkpoint provenance triggered restore: %+v", decoded.Items)
	}
}
