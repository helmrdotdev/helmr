package agent_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"

	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestComputerProcessListingPreservesCustodyWithoutExecutionAuthority(t *testing.T) {
	f, _, e := startingSessionFixture(t)
	p, err := agent.ReadProcessAllocation(t.Context(), f.pool, f.env, e.SessionID, e.ProcessEpoch)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: p.ComputerID, InstanceID: p.InstanceID, Epoch: p.LeaseEpoch}
	assertListed := func() {
		t.Helper()
		rows, err := dispatch.ListHostProcesses(t.Context(), f.pool, *f.host(), identity, nil)
		if err != nil || len(rows) != 1 || rows[0].SessionID != e.SessionID || rows[0].Epoch != e.ProcessEpoch {
			t.Fatalf("owned process listing: %+v %v", rows, err)
		}
		end, err := dispatch.ListHostProcesses(t.Context(), f.pool, *f.host(), identity, &rows[0])
		if err != nil || len(end) != 0 {
			t.Fatalf("cursor: %+v %v", end, err)
		}
	}
	assertListed()
	attachment, err := agent.AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.ObserveSessionFailure(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	assertListed()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second',status='lost' WHERE environment_id=$1 AND computer_id=$2`, f.env, p.ComputerID)
	assertListed()
	wrong := identity
	wrong.InstanceID = uuid.NewV7()
	if _, err := dispatch.ListHostProcesses(t.Context(), f.pool, *f.host(), wrong, nil); !errors.Is(err, agent.ErrDenied) {
		t.Fatalf("foreign instance: %v", err)
	}
	host := *f.host()
	host.Epoch++
	if rows, err := dispatch.ListHostProcesses(t.Context(), f.pool, host, identity, nil); err == nil || len(rows) != 0 {
		t.Fatalf("foreign incarnation: %+v %v", rows, err)
	}
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch.ListHostProcesses(t.Context(), f.pool, *f.host(), identity, nil); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("stopped allocation: %v", err)
	}
}
