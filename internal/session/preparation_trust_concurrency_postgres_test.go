package session_test

import (
	"bytes"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

type countingPreparationTrustIssuer struct {
	dispatch.ImageBuildTrustIssuer
	calls atomic.Int32
}

func (i *countingPreparationTrustIssuer) GeneratePreparationProxyTrust(env, attempt uuid.UUID, now, deadline time.Time) (secret.ProxyTrust, error) {
	i.calls.Add(1)
	return i.ImageBuildTrustIssuer.GeneratePreparationProxyTrust(env, attempt, now, deadline)
}

func TestPreparationTrustConcurrentAllocationCommitsOneSigner(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	marker, err := secretbinding.Placeholder("protected")
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE secret_bindings SET mode='protected',placeholder=$3,allowed_origins=ARRAY['https://api.example.com'] WHERE environment_id=$1 AND image_spec_id=$2 AND placement_target='TOKEN'`, f.env, p.SpecID, marker)
	issuer := &countingPreparationTrustIssuer{ImageBuildTrustIssuer: f.secrets}
	a = testAllocator(t, f.pool, issuer)
	const callers = 8
	start := make(chan struct{})
	results := make(chan computer.Allocation, callers)
	failures := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Go(func() {
			<-start
			result, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
			results <- result
			failures <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil && !errors.Is(err, session.ErrNotReady) {
			t.Fatal(err)
		}
	}
	committed, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	for result := range results {
		if result.InstanceID != uuid.Nil() && result.InstanceID != committed.InstanceID {
			t.Fatal("retry created another executor")
		}
	}
	if issuer.calls.Load() != 1 {
		t.Fatalf("signer generated %d times for one committed allocation", issuer.calls.Load())
	}
	var before []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT proxy_ca_private_key_ciphertext FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&before); err != nil || len(before) == 0 {
		t.Fatalf("committed signer: %v", err)
	}
	restarted, err := dispatch.NewAllocator(f.pool, auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, bytes.Repeat([]byte{19}, 32), issuer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker); err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT proxy_ca_private_key_ciphertext FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&after); err != nil || !bytes.Equal(before, after) || issuer.calls.Load() != 1 {
		t.Fatal("restart replaced committed signer")
	}
}
