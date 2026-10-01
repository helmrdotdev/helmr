package controlplane

import (
	"net/http/httptest"
	"testing"
	"time"

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
