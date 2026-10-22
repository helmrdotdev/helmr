package session_test

import (
	"bytes"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

// Exercise the real published-generation fixture, including its certified image
// key closure. Mutations are restored so the owning publication test continues.
func checkPreparationSourceAuthorization(t *testing.T, p *preparationPublicationTest, build uuid.UUID) {
	t.Helper()
	f := p.f
	key := computer.PreparationKey{EnvironmentID: f.env, BuildID: build}
	provider := &preparationKeyProvider{DataKeyWrapper: preparationWrapper(t)}
	broker, err := computer.NewKeyBroker(f.pool, provider)
	if err != nil {
		t.Fatal(err)
	}
	acknowledge := func() {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts h SET
 resource_observation_sequence=1,resource_policy_revision=p.resource_policy_revision,resource_observed_at=clock_timestamp(),
 local_optional_cpu_millis=0,local_optional_memory_bytes=0,optional_disk_bytes=0,local_optional_vm_slots=0,local_optional_tasks=0,
 optional_reclaim_pending=false,disk_slots_for_real_work=1,ready_leaf_keys='[]'::jsonb
 FROM worker_pools p WHERE h.id=$1 AND p.id=h.worker_pool_id`, f.worker)
	}
	acknowledge()
	hints, err := computer.ListPreparationSources(t.Context(), f.pool, *f.host())
	if err != nil || len(hints) != 1 || hints[0] != key {
		t.Fatalf("qualified hints: %v %v", hints, err)
	}
	source, err := broker.PreparationSource(t.Context(), *f.host(), key)
	if err != nil {
		t.Fatal(err)
	}
	if source.Key != key || source.HostID != f.worker || source.HostEpoch != f.host().Epoch || source.Disk.WriteKeyID != uuid.Nil() || len(source.Disk.Keys) == 0 || time.Until(source.ExpiresAt) <= 0 || time.Until(source.ExpiresAt) > 120*time.Second {
		t.Fatalf("invalid source envelope: key=%v host=%v epoch=%d", source.Key, source.HostID, source.HostEpoch)
	}
	source.Disk.Clear()
	stale := *f.host()
	stale.Epoch++
	if s, err := broker.PreparationSource(t.Context(), stale, key); err == nil || len(s.Disk.Keys) > 0 {
		t.Fatal("stale host received source")
	}
	other := key
	other.EnvironmentID = uuid.NewV7()
	if s, err := broker.PreparationSource(t.Context(), *f.host(), other); err == nil || len(s.Disk.Keys) > 0 {
		t.Fatal("other Environment received source")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET resource_observed_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, f.worker)
	if s, err := broker.PreparationSource(t.Context(), *f.host(), key); err == nil || len(s.Disk.Keys) > 0 {
		t.Fatal("stale bounds received source")
	}
	acknowledge()
	// A revision change while the key service runs must erase the unwrapped copy.
	provider.beforeUnwrap = func() {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET resource_policy_revision=resource_policy_revision+1 WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.worker)
	}
	if s, err := broker.PreparationSource(t.Context(), *f.host(), key); err == nil || len(s.Disk.Keys) > 0 {
		t.Fatal("policy change during unwrap delivered plaintext")
	}
	if len(provider.lastPlain) == 0 || !bytes.Equal(provider.lastPlain, make([]byte, len(provider.lastPlain))) {
		t.Fatal("denied plaintext was retained")
	}
	provider.beforeUnwrap = nil
	acknowledge()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET optional_max_tasks=0 WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.worker)
	hints, err = computer.ListPreparationSources(t.Context(), f.pool, *f.host())
	if err != nil || len(hints) != 0 {
		t.Fatalf("zero policy advertised sources: %v %v", hints, err)
	}
	if s, err := broker.PreparationSource(t.Context(), *f.host(), key); err == nil || len(s.Disk.Keys) > 0 {
		t.Fatal("zero policy delivered source")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET optional_max_tasks=max_vm_slots WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.worker)
	acknowledge()
}
