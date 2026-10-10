package workergroup

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestRuntimeHostAdmissionPreservesLifecycleAndCredentialDistinctions(t *testing.T) {
	f := newSupplyFixture(t)
	host := f.start(t, testHostAuthConfig(t), "default", "i-runtime", validHostTemplate(t)).principal
	for _, gs := range []string{"active", "paused", "draining", "disabled"} {
		for _, hs := range []string{"active", "draining", "registering", "termination_ready", "lost"} {
			t.Run(gs+"/"+hs, func(t *testing.T) {
				tx := f.begin(t)
				dbtest.MustExec(t, t.Context(), tx, `UPDATE worker_groups SET status=$2 WHERE id=$1`, host.GroupID, gs)
				dbtest.MustExec(t, t.Context(), tx, `UPDATE worker_hosts SET status=$2, drain_reason=CASE WHEN $2 IN ('draining','termination_ready') THEN 'admin' END, draining_at=CASE WHEN $2 IN ('draining','termination_ready') THEN clock_timestamp() END, termination_ready_at=CASE WHEN $2='termination_ready' THEN clock_timestamp() END, lost_at=CASE WHEN $2='lost' THEN clock_timestamp() END WHERE id=$1`, host.HostID, hs)
				eligible, err := LockRuntimeHost(t.Context(), tx, host)
				want := gs != "disabled" && (hs == "active" || hs == "draining")
				if err != nil || eligible != want {
					t.Fatalf("eligible=%v err=%v want=%v", eligible, err, want)
				}
			})
		}
	}
	for _, change := range []struct {
		name   string
		mutate func(*HostPrincipal)
		want   error
	}{
		{"host claims", func(p *HostPrincipal) { p.HostClaimVersion++ }, ErrStaleClaims},
		{"group claims", func(p *HostPrincipal) { p.GroupClaimVersion++ }, ErrStaleClaims},
		{"old epoch", func(p *HostPrincipal) { p.Epoch++ }, nil},
		{"missing host", func(p *HostPrincipal) { p.HostID = uuid.NewV7() }, pgx.ErrNoRows},
		{"missing group", func(p *HostPrincipal) { p.GroupID = uuid.NewV7() }, pgx.ErrNoRows},
	} {
		t.Run(change.name, func(t *testing.T) {
			p := host
			change.mutate(&p)
			eligible, err := LockRuntimeHost(t.Context(), f.begin(t), p)
			if eligible || !errors.Is(err, change.want) {
				t.Fatalf("eligible=%v err=%v want=%v", eligible, err, change.want)
			}
		})
	}
}

func TestRuntimeHostAdmissionRechecksClaimsAfterLockWait(t *testing.T) {
	for _, table := range []string{"worker_groups", "worker_hosts"} {
		t.Run(table, func(t *testing.T) {
			f := newSupplyFixture(t)
			host := f.start(t, testHostAuthConfig(t), "default", "i-runtime-wait", validHostTemplate(t)).principal
			id := host.GroupID
			if table == "worker_hosts" {
				id = host.HostID
			}
			blocker := f.begin(t)
			dbtest.MustExec(t, t.Context(), blocker, `UPDATE `+table+` SET claim_version=claim_version+1 WHERE id=$1`, id)
			reader := f.begin(t)
			result := make(chan error, 1)
			go func() {
				eligible, err := LockRuntimeHost(t.Context(), reader, host)
				if eligible {
					err = errors.New("revoked credentials admitted")
				}
				result <- err
			}()
			waitForBlockedQuery(t, f.pool, "FROM "+table, 1)
			if err := blocker.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, ErrStaleClaims) {
				t.Fatalf("admission after claim revocation: %v", err)
			}
		})
	}
}
