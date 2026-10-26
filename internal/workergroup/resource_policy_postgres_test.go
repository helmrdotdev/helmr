package workergroup

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestResourcePolicyPostgresRevisionAndScope(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "policy")
	update := ResourcePolicyUpdate{ExpectedRevision: 1, ResourcePolicy: ResourcePolicy{Optional: OptionalResources{DiskBytes: 4096, MaxTasks: 1, IOBytesPerSecond: 1024}}}
	result, err := UpdateResourcePolicy(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), f.groupID(), pool.ID.Bytes, update)
	if err != nil || result.Revision != 2 || result.ResourcePolicy != update.ResourcePolicy {
		t.Fatalf("update: %+v %v", result, err)
	}
	var stale PolicyRevisionConflict
	if _, err := UpdateResourcePolicy(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), f.groupID(), pool.ID.Bytes, update); !errors.As(err, &stale) || stale.CurrentRevision != 2 {
		t.Fatalf("stale: %v", err)
	}
	update.ExpectedRevision = 2
	same, err := UpdateResourcePolicy(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), f.groupID(), pool.ID.Bytes, update)
	if err != nil || same != result {
		t.Fatalf("no-op: %+v %v", same, err)
	}
	if _, err := GetResourcePolicy(t.Context(), f.q, uuid.NewV7(), pool.ID.Bytes); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("scope: %v", err)
	}
	var audits int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='worker_pool.resource_policy_updated'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit: %d %v", audits, err)
	}
	var claim int64
	if err := f.pool.QueryRow(t.Context(), `SELECT claim_version FROM worker_pools WHERE id=$1`, pool.ID).Scan(&claim); err != nil || claim != pool.ClaimVersion {
		t.Fatalf("claim changed: %d %v", claim, err)
	}
}

func TestResourcePolicyPostgresConcurrentAndBounds(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "policy")
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			<-start
			_, errs[i] = UpdateResourcePolicy(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), f.groupID(), pool.ID.Bytes, ResourcePolicyUpdate{ExpectedRevision: 1, ResourcePolicy: ResourcePolicy{Optional: OptionalResources{DiskBytes: int64(i + 1)}}})
		})
	}
	close(start)
	wg.Wait()
	success := 0
	for _, err := range errs {
		if err == nil {
			success++
		} else {
			var stale PolicyRevisionConflict
			if !errors.As(err, &stale) {
				t.Fatal(err)
			}
		}
	}
	if success != 1 {
		t.Fatalf("concurrent: %v", errs)
	}
	for _, policy := range []ResourcePolicy{{Optional: OptionalResources{CPUMillis: pool.CapacityCPUMillis.Int64 + 1}}, {Optional: OptionalResources{MemoryBytes: -1}}, {LocalHeadroomSlots: pool.MaxVMSlots.Int32 + 1}, {Optional: OptionalResources{DiskBytes: 1 << 53}}} {
		var input InputError
		if _, err := UpdateResourcePolicy(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), f.groupID(), pool.ID.Bytes, ResourcePolicyUpdate{ExpectedRevision: 2, ResourcePolicy: policy}); !errors.As(err, &input) {
			t.Fatalf("bounds %+v: %v", policy, err)
		}
	}
	pending := f.pendingPool(t, "pending")
	if _, err := UpdateResourcePolicy(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), f.groupID(), pending.ID.Bytes, ResourcePolicyUpdate{ExpectedRevision: 1, ResourcePolicy: ResourcePolicy{Optional: OptionalResources{DiskBytes: 1}}}); err == nil {
		t.Fatal("unsealed policy accepted")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET status='draining' WHERE id=$1`, pool.ID)
	if _, err := UpdateResourcePolicy(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), f.groupID(), pool.ID.Bytes, ResourcePolicyUpdate{ExpectedRevision: 2, ResourcePolicy: ResourcePolicy{Optional: OptionalResources{DiskBytes: 4096}}}); err != nil {
		t.Fatal(err)
	}
}
