package session_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestHostEnrollmentRetainsLostResourceCustody(t *testing.T) {
	for _, kind := range []string{"computer", "preparation", "both"} {
		t.Run(kind, func(t *testing.T) {
			f := newPreparationFixture(t)
			p := f.attach(t, f.waiter(t))
			ref := f.claim(t, p)
			if kind == "computer" {
				if err := computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), preparationIdentity(ref)); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "preparation" {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='fixture VM physically stopped' WHERE worker_host_id=$1`, f.worker)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp(),claim_version=claim_version+1 WHERE id=$1;
 UPDATE computer_leases SET status='lost' WHERE worker_host_id=$1 AND fenced_at IS NULL;
 UPDATE image_builds SET status='failed',error_code='executor_lost' WHERE worker_host_id=$1 AND fenced_at IS NULL`, pgx.QueryExecModeSimpleProtocol, f.worker)
			request := fixtureEnrollment(t, "host")
			if _, err := workergroup.EnrollHost(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, request); !errors.Is(err, workergroup.ErrHostCustody) {
				t.Fatalf("lost resource enrollment: %v", err)
			}
			q := db.New(f.pool)
			listed, err := q.ListCapacityWorkerHosts(t.Context(), db.ListCapacityWorkerHostsParams{WorkerGroupID: pgvalue.UUID(f.group), ResourceIds: []string{"host"}, Statuses: []string{}, RowLimit: 10})
			if err != nil || len(listed) != 1 || pgvalue.MustUUIDValue(listed[0].ID) != f.worker || listed[0].Status != "lost" {
				t.Fatalf("provider lost-host visibility: %d rows: %v", len(listed), err)
			}
			other := fixtureEnrollment(t, "unrelated-host")
			if _, err := workergroup.EnrollHost(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, other); err != nil {
				t.Fatalf("custody blocked a different physical resource: %v", err)
			}
			if err := confirmComputerHostAbsent(t.Context(), f.fixture, f.worker); err != nil {
				t.Fatal(err)
			}
			registered, err := workergroup.EnrollHost(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, request)
			if err != nil || registered.HostID == f.worker || registered.HostID == uuid.Nil() {
				t.Fatalf("fresh enrollment after physical absence: %v", err)
			}
		})
	}
}

// fixtureEnrollment enrolls a resource in the fixture group's pool with a new
// machine key.
func fixtureEnrollment(t *testing.T, resourceID string) workergroup.Enrollment {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return workergroup.Enrollment{
		Token: auth.ParsedEnrollmentToken{Hash: make([]byte, 32)}, PoolName: "pool", ResourceID: resourceID, PublicKey: publicKey,
	}
}
