package controlplane

import (
	"bytes"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func TestWorkerDrainReauthenticatesDuringActiveWork(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	secret := "drain-test-secret"
	hash, err := auth.HashToken(keys.WorkerInstance, secret)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, serviceID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_instances SET current_service_id=$2 WHERE id=$1`, f.WorkerID, serviceID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_instance_credentials
 (id,worker_group_id,worker_instance_id,key_prefix,secret_hash)
 VALUES ($1,$2,$3,'drain-test',$4)`, credentialID, runtest.WorkerGroupID, f.WorkerID, hash)
	s := &Server{
		db: db.New(f.Pool), tx: f.Pool, authKeys: keys,
		workerTokenSigningKey: bytes.Repeat([]byte{2}, auth.RootKeySize), workerTokenTTL: time.Hour,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	router := chi.NewRouter()
	s.mountWorkerRoutes(router)
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()

	var firstDrainingAt time.Time
	for attempt := range 3 {
		// Each CLI invocation exchanges the same Service credential for a fresh
		// token, so the second drain carries the current post-transition claim.
		client, err := workerclient.New(httpServer.URL, workerclient.WithAuth(f.WorkerID.String(), secret), workerclient.WithService(serviceID.String()))
		if err != nil {
			t.Fatal(err)
		}
		status, err := client.DrainWorker(t.Context())
		if err != nil {
			t.Fatalf("drain invocation %d: %v", attempt+1, err)
		}
		if status.Status != workerapi.StatusDraining || status.ActiveExecutions == 0 {
			t.Fatalf("drain with active work: %+v", status)
		}
		var claim, credentialClaim int64
		var drainingAt time.Time
		var revoked bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,c.claim_version,w.draining_at,c.revoked_at IS NOT NULL
 FROM worker_instances w JOIN worker_instance_credentials c ON c.worker_instance_id=w.id
 WHERE w.id=$1 AND c.id=$2`, f.WorkerID, credentialID).Scan(&claim, &credentialClaim, &drainingAt, &revoked); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			firstDrainingAt = drainingAt
		}
		if claim != 2 || credentialClaim != 2 || revoked || !drainingAt.Equal(firstDrainingAt) {
			t.Fatalf("reentry changed transition: worker=%d credential=%d revoked=%v draining_at=%s", claim, credentialClaim, revoked, drainingAt)
		}
	}
	var leaseStatus, desiredState string
	if err := f.Pool.QueryRow(t.Context(), `SELECT l.status,r.desired_state FROM run_leases l
 JOIN runtime_instances r ON r.id=l.runtime_instance_id WHERE l.id=$1`, work.LeaseID).Scan(&leaseStatus, &desiredState); err != nil {
		t.Fatal(err)
	}
	if leaseStatus != "starting" || desiredState != "ready" {
		t.Fatalf("drain changed active execution: lease=%s runtime=%s", leaseStatus, desiredState)
	}
}
