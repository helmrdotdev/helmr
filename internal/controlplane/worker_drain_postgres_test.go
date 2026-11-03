package controlplane

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerDrainReauthenticatesDuringActiveWork(t *testing.T) {
	f := sessiontest.New(t)
	hostKey := seedHostKey(t, f.Pool, f.Worker)
	httpServer := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer httpServer.Close()

	var firstDrainingAt time.Time
	for attempt := range 3 {
		// Each CLI invocation answers a challenge with the same machine key
		// for a fresh host credential, so the second drain carries the current post-transition claim.
		status, err := hostKey.client(t, httpServer.URL).DrainWorker(t.Context())
		if err != nil {
			t.Fatalf("drain invocation %d: %v", attempt+1, err)
		}
		if status.Status != workerapi.StatusDraining || status.ActiveInstances == 0 {
			t.Fatalf("drain with active work: %+v", status)
		}
		var claim, hostKeyClaim int64
		var drainingAt time.Time
		var revoked bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,c.claim_version,w.draining_at,c.revoked_at IS NOT NULL
 FROM worker_hosts w JOIN worker_host_keys c ON c.worker_host_id=w.id
 WHERE w.id=$1 AND c.id=$2`, f.Worker, hostKey.keyID).Scan(&claim, &hostKeyClaim, &drainingAt, &revoked); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			firstDrainingAt = drainingAt
		}
		if claim != 2 || hostKeyClaim != 2 || revoked || !drainingAt.Equal(firstDrainingAt) {
			t.Fatalf("reentry changed transition: worker=%d host_key=%d revoked=%v draining_at=%s", claim, hostKeyClaim, revoked, drainingAt)
		}
	}
	var preserved bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='active' AND fenced_at IS NULL FROM computer_leases WHERE computer_id=$1`, f.Computer).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("drain changed physical allocation: %v %v", preserved, err)
	}
}
