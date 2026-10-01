package controlplane

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerDrainReauthenticatesDuringActiveWork(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	hostSecret := seedHostSecret(t, f.Pool, f.WorkerID)
	httpServer := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer httpServer.Close()

	var firstDrainingAt time.Time
	for attempt := range 3 {
		// Each CLI invocation exchanges the same host secret for a fresh
		// host credential, so the second drain carries the current post-transition claim.
		status, err := hostSecret.client(t, httpServer.URL).DrainWorker(t.Context())
		if err != nil {
			t.Fatalf("drain invocation %d: %v", attempt+1, err)
		}
		if status.Status != workerapi.StatusDraining || status.ActiveInstances == 0 {
			t.Fatalf("drain with active work: %+v", status)
		}
		var claim, hostSecretClaim int64
		var drainingAt time.Time
		var revoked bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,c.claim_version,w.draining_at,c.revoked_at IS NOT NULL
 FROM worker_hosts w JOIN worker_host_secrets c ON c.worker_host_id=w.id
 WHERE w.id=$1 AND c.key_prefix=$2`, f.WorkerID, hostSecret.secret).Scan(&claim, &hostSecretClaim, &drainingAt, &revoked); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			firstDrainingAt = drainingAt
		}
		if claim != 2 || hostSecretClaim != 2 || revoked || !drainingAt.Equal(firstDrainingAt) {
			t.Fatalf("reentry changed transition: worker=%d host_secret=%d revoked=%v draining_at=%s", claim, hostSecretClaim, revoked, drainingAt)
		}
	}
	var leaseStatus, desiredState string
	if err := f.Pool.QueryRow(t.Context(), `SELECT l.status,r.desired_state FROM run_leases l
 JOIN computer_instances r ON r.id=l.computer_instance_id WHERE l.id=$1`, work.LeaseID).Scan(&leaseStatus, &desiredState); err != nil {
		t.Fatal(err)
	}
	if leaseStatus != "starting" || desiredState != "ready" {
		t.Fatalf("drain changed active execution: lease=%s instance=%s", leaseStatus, desiredState)
	}
}

func TestWorkerDrainImmediatelyCapturesSafeResidents(t *testing.T) {
	f, _, _, capture := computertest.Capture(t)
	hostSecret := seedHostSecret(t, f.Pool, f.WorkerID)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	if _, err := hostSecret.client(t, server.URL).DrainWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	var captured bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT admission_state='checkpointing' AND capture_checkpoint_id IS NOT NULL FROM computer_instances WHERE id=$1`, capture.InstanceID).Scan(&captured); err != nil || !captured {
		t.Fatalf("capture after drain=%v %v", captured, err)
	}
}

func TestWaitRegistrationImmediatelyCapturesDrainingComputer(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, work.RunID)
	hostSecret := seedHostSecret(t, f.Pool, f.WorkerID)
	handler := newPostgresServer(t, f.Pool)
	server := httptest.NewServer(handler)
	defer server.Close()
	if _, err := hostSecret.client(t, server.URL).DrainWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	var captured bool
	query := `SELECT capture_checkpoint_id IS NOT NULL FROM computer_instances WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`
	if err := f.Pool.QueryRow(t.Context(), query, work.LeaseID).Scan(&captured); err != nil || captured {
		t.Fatalf("active member captured=%v %v", captured, err)
	}
	client := newWorkerHTTPClient(t, handler, f.Pool, f.WorkerID)
	timeout := int64(3600000)
	request := workerapi.CreateRunWaitRequest{Lease: workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1}, CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), Kind: workerapi.RunWaitKindTimer, Params: json.RawMessage(`{"duration":"1h"}`), TimeoutMS: &timeout, IdleTimeoutMS: &timeout}
	client.post(t, "/worker/v1/run/waits/create", request, http.StatusOK, nil)
	if err := f.Pool.QueryRow(t.Context(), query, work.LeaseID).Scan(&captured); err != nil || !captured {
		t.Fatalf("new safe wait not captured=%v %v", captured, err)
	}
}
