package agent

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

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
				if err := ObservePreparationStopped(t.Context(), f.pool, *f.host(), ref.Identity()); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "preparation" {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='fixture VM physically stopped' WHERE worker_host_id=$1`, f.worker)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp(),claim_version=claim_version+1 WHERE id=$1;
 UPDATE computer_leases SET status='lost' WHERE worker_host_id=$1 AND fenced_at IS NULL;
 UPDATE computer_preparations SET status='failed',error_code='executor_lost' WHERE worker_host_id=$1 AND fenced_at IS NULL`, pgx.QueryExecModeSimpleProtocol, f.worker)
			cfg, err := workergroup.NewHostAuthConfig(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			request := workergroup.Enrollment{TokenHash: make([]byte, 32), PoolName: "pool", ResourceID: "host"}
			if _, err := workergroup.EnrollHost(t.Context(), f.pool, cfg, request); !errors.Is(err, workergroup.ErrHostCustody) {
				t.Fatalf("lost resource enrollment: %v", err)
			}
			q := db.New(f.pool)
			listed, err := q.ListCapacityWorkerHosts(t.Context(), db.ListCapacityWorkerHostsParams{WorkerGroupID: pgvalue.UUID(f.group), ResourceIds: []string{"host"}, Statuses: []string{}, RowLimit: 10})
			if err != nil || len(listed) != 1 || pgvalue.MustUUIDValue(listed[0].ID) != f.worker || listed[0].Status != "lost" {
				t.Fatalf("provider lost-host visibility: %d rows: %v", len(listed), err)
			}
			other := request
			other.ResourceID = "unrelated-host"
			if _, err := workergroup.EnrollHost(t.Context(), f.pool, cfg, other); err != nil {
				t.Fatalf("custody blocked a different physical resource: %v", err)
			}
			if err := confirmComputerHostAbsent(t.Context(), f.fixture, f.worker); err != nil {
				t.Fatal(err)
			}
			registered, err := workergroup.EnrollHost(t.Context(), f.pool, cfg, request)
			if err != nil || registered.HostID == f.worker || registered.HostID == uuid.Nil() {
				t.Fatalf("fresh enrollment after physical absence: %v", err)
			}
		})
	}
}
