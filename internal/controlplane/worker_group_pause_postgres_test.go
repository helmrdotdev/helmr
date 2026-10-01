package controlplane

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

// A paused Worker Group stops admission only. Workers with refreshed claims
// keep resolving protected Secrets and reading Session control for started work.
func TestPausedWorkerGroupKeepsStartedWorkerAuthority(t *testing.T) {
	t.Run("protected Secrets", func(t *testing.T) {
		f := newSnapshotFixture(t, 1, true)
		dbtest.MustExec(t, t.Context(), f.fixture.Pool, `UPDATE worker_groups SET status='paused' WHERE id=$1`, f.worker.GroupID)
		for _, resolve := range []bool{false, true} {
			if response := f.invoke(t.Context(), resolve); response.Code != 200 {
				t.Fatalf("resolve=%v on paused Group: %d %s", resolve, response.Code, response.Body.String())
			}
		}
	})
	t.Run("allocated Secret preparation", func(t *testing.T) {
		for _, test := range []struct {
			name, sql string
			allowed   bool
		}{
			{"active", ``, true},
			{"paused Group", `UPDATE worker_groups SET status='paused' WHERE id=$1`, false},
			{"draining Host", `UPDATE worker_hosts SET status='draining',draining_at=now() WHERE worker_group_id=$1`, false},
			{"draining Pool", `WITH g AS (UPDATE worker_groups SET primary_pool_id=NULL WHERE id=$1 RETURNING id) UPDATE worker_pools SET status='draining' WHERE worker_group_id=(SELECT id FROM g)`, false},
			{"Run paused Host", `UPDATE worker_hosts SET run_paused_reason='startup_recovery_leak' WHERE worker_group_id=$1`, false},
			{"VM paused Host", `UPDATE worker_hosts SET vm_paused_reason='runtime_health' WHERE worker_group_id=$1`, false},
		} {
			t.Run(test.name, func(t *testing.T) {
				f := newSnapshotFixture(t, 1, true)
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_desired_version=0,ready_at=NULL,preparation_expires_at=now()+interval '5 minutes' WHERE id=$1`, f.runtime)
				if test.sql != "" {
					dbtest.MustExec(t, t.Context(), f.fixture.Pool, test.sql, f.worker.GroupID)
				}
				if response := f.invoke(t.Context(), false); (response.Code == 200) != test.allowed {
					t.Fatalf("allocated preparation: %d %s", response.Code, response.Body.String())
				}
			})
		}
	})
	t.Run("Session control", func(t *testing.T) {
		f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
		scope := f.receiveTurn(t, 1)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET status='paused' WHERE id=$1`, f.worker.GroupID)
		state, err := f.server.db.ReadWorkerSessionControl(t.Context(), db.ReadWorkerSessionControlParams{RunLeaseID: f.claim.Lease().ID, LeaseSequence: f.fence().LeaseSequence, WorkerGroupID: pgvalue.UUID(f.worker.GroupID), WorkerHostID: pgvalue.UUID(f.worker.HostID), WorkerEpoch: f.worker.Epoch, RunGeneration: scope.RunGeneration})
		if err != nil || state.ActiveTurnID != pgvalue.UUID(scope.TurnID) {
			t.Fatalf("Session control on paused Group: %+v %v", state, err)
		}
	})
}
