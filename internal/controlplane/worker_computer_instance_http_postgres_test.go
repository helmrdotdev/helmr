package controlplane

import (
	"net/http"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// instanceHTTPFixture serves NewServer to the worker host of a running
// lease's Instance.
func instanceHTTPFixture(t *testing.T) (runtest.Fixture, workerHTTPClient, workerapi.ComputerInstanceRenewRequest) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
	worker := newWorkerHTTPClient(t, newPostgresServer(t, f.Pool), f.Pool, f.WorkerID)
	request := workerapi.ComputerInstanceRenewRequest{EnvironmentID: f.EnvironmentID.String()}
	var instance uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id,writer_generation FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&instance, &request.WriterGeneration); err != nil {
		t.Fatal(err)
	}
	request.ComputerInstanceID = instance.String()
	return f, worker, request
}

func TestComputerInstanceClaimRouteProjectsFreshAuthority(t *testing.T) {
	_, worker, instance := instanceHTTPFixture(t)
	worker.post(t, "/worker/v1/run/computer-instances/claim", map[string]string{"computer_mount_id": "obsolete"}, http.StatusBadRequest, nil)
	var response workerapi.ComputerInstanceClaimResponse
	worker.post(t, "/worker/v1/run/computer-instances/claim", struct{}{}, http.StatusOK, &response)
	i := response.Assignment
	if i == nil || i.ComputerInstanceID != instance.ComputerInstanceID || i.WriterGeneration != instance.WriterGeneration || i.DesiredVersion != 1 || i.ObservedVersion != 1 || i.Target.BaseComputerDiskVersionID == "" || i.RestoreCheckpointID != "" || i.GuestdChannelToken == "" {
		t.Fatal("fresh Instance authority incorrectly projected")
	}
	if out := worker.post(t, "/worker/v1/run/computer-instances/claim", struct{}{}, http.StatusOK, nil); out.Body.String() != "{}\n" {
		t.Fatalf("second claim=%s", out.Body.String())
	}
}

// Instance routes report changed authority as 409 and rejected input as 400.
func TestComputerInstanceRoutesMapOwnerErrors(t *testing.T) {
	f, worker, instance := instanceHTTPFixture(t)
	var renewed workerapi.ComputerInstanceRenewResponse
	worker.post(t, "/worker/v1/run/computer-instances/renew", instance, http.StatusOK, &renewed)
	if renewed.ComputerInstanceID != instance.ComputerInstanceID || renewed.WriterGeneration != instance.WriterGeneration {
		t.Fatalf("renewal = %+v", renewed)
	}
	stale := instance
	stale.WriterGeneration++
	worker.post(t, "/worker/v1/run/computer-instances/renew", stale, http.StatusConflict, nil)
	cleanup := workerapi.ComputerRunCleanupRequest{EnvironmentID: instance.EnvironmentID, ComputerInstanceID: instance.ComputerInstanceID, WriterGeneration: stale.WriterGeneration}
	worker.post(t, "/worker/v1/run/computer-instances/runs/cleanup", cleanup, http.StatusConflict, nil)
	cleanup.WriterGeneration = instance.WriterGeneration
	var processes workerapi.ComputerRunCleanupResponse
	worker.post(t, "/worker/v1/run/computer-instances/runs/cleanup", cleanup, http.StatusOK, &processes)
	if processes.Run != nil {
		t.Fatalf("running lease offered for cleanup: %+v", processes.Run)
	}
	plan := workerapi.ComputerRestorePlanRequest(instance)
	worker.post(t, "/worker/v1/computer/restores/plan", plan, http.StatusConflict, nil)

	observation := workerapi.ComputerInstanceStateRequest{ID: instance.ComputerInstanceID, WorkerEpoch: 1, DesiredVersion: 1, ExpectedObservedVersion: 1}
	worker.post(t, "/worker/v1/run/computer-instances/closed", observation, http.StatusBadRequest, nil)
	observation.CleanupProof = &workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupNotMaterialized, CompletedAt: time.Now()}
	worker.post(t, "/worker/v1/run/computer-instances/closed", observation, http.StatusBadRequest, nil)
	observation.CleanupProof.Method = workerapi.RuntimeCleanupSessionClosed
	worker.post(t, "/worker/v1/run/computer-instances/closed", observation, http.StatusConflict, nil)
	observation.CleanupProof = nil
	observation.ExpectedObservedVersion = 0
	worker.post(t, "/worker/v1/run/computer-instances/failed", observation, http.StatusConflict, nil)
	observation.ExpectedObservedVersion = 1
	observation.ReasonCode = "runtime_reconcile_failed"
	var failed workerapi.ComputerInstance
	worker.post(t, "/worker/v1/run/computer-instances/failed", observation, http.StatusOK, &failed)
	if failed.Status != "failed" {
		t.Fatalf("failed observation = %+v", failed)
	}
	var unreclaimed bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT reclaimed_at IS NULL FROM computer_instances WHERE id=$1`, instance.ComputerInstanceID).Scan(&unreclaimed); err != nil || !unreclaimed {
		t.Fatalf("failure without proof reclaimed the Instance: %v %v", unreclaimed, err)
	}
}
