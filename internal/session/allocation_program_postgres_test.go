package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestAllocationProgramIsBytesOnlyAndStableBeforeReady(t *testing.T) {
	f, _, e := startingSessionFixture(t)
	p, err := session.ReadProcessAllocation(t.Context(), f.pool, f.env, e.SessionID, e.ProcessEpoch)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: p.ComputerID, InstanceID: p.InstanceID, Epoch: p.LeaseEpoch}
	// The normal fixture has already observed readiness. Before that observation,
	// the same admitted cohort is visible without any execution attachment.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='acquiring',initialized_at=NULL WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, f.env, p.ComputerID, p.LeaseEpoch)
	first, err := session.ReadAllocationProgram(t.Context(), f.pool, *f.host(), identity)
	if err != nil || first == nil {
		t.Fatalf("initial pair: %+v %v", first, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, f.env, e.SessionID, e.ProcessEpoch)
	again, err := session.ReadAllocationProgram(t.Context(), f.pool, *f.host(), identity)
	if err != nil || again == nil || *again != *first {
		t.Fatalf("fencing changed bytes: %+v %v", again, err)
	}
	wrong := identity
	wrong.InstanceID = uuid.NewV7()
	if _, err := session.ReadAllocationProgram(t.Context(), f.pool, *f.host(), wrong); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("foreign allocation: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='active',initialized_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, f.env, p.ComputerID, p.LeaseEpoch)
	if later, err := session.ReadAllocationProgram(t.Context(), f.pool, *f.host(), identity); err != nil || later != nil {
		t.Fatalf("selected after boot: %+v %v", later, err)
	}
}
