package workergroup

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestResourceObservationPostgresSequenceAndEpoch(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "observe")
	host := f.activeHost(t, pool, "host")
	principal := HostPrincipal{HostID: host, GroupID: f.groupID(), Epoch: 1}
	observation := HostObservation{Resources: workerapi.ResourceObservation{Sequence: 2, PolicyRevision: 1, OptionalDiskBytes: 4096, DiskSlotsForRealWork: 1}}
	if err := RecordObservation(t.Context(), f.q, principal, observation); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute).UTC()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET resource_observed_at=$2,observed_at=$2 WHERE id=$1`, host, old)
	observation.Resources.OptionalDiskBytes = 0
	for _, seq := range []int64{2, 1} {
		observation.Resources.Sequence = seq
		if err := RecordObservation(t.Context(), f.q, principal, observation); err != nil {
			t.Fatal(err)
		}
	}
	var disk int64
	var resourceTime, hostTime time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT optional_disk_bytes,resource_observed_at,observed_at FROM worker_hosts WHERE id=$1`, host).Scan(&disk, &resourceTime, &hostTime); err != nil {
		t.Fatal(err)
	}
	if disk != 4096 || resourceTime.Sub(old).Abs() > time.Microsecond || hostTime.Sub(old).Abs() > time.Microsecond {
		t.Fatalf("duplicate changed observations: %d %v %v", disk, resourceTime, hostTime)
	}
	observation.Resources.Sequence = 3
	if err := RecordObservation(t.Context(), f.q, principal, observation); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT optional_disk_bytes FROM worker_hosts WHERE id=$1`, host).Scan(&disk); err != nil || disk != 0 {
		t.Fatalf("new sequence: %d %v", disk, err)
	}
	observation.Resources.Sequence = 4
	observation.Resources.LocalOptional.MemoryBytes = -1
	if err := RecordObservation(t.Context(), f.q, principal, observation); err == nil {
		t.Fatal("negative observation accepted")
	}
}
