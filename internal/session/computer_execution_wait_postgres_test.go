package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
)

func executionWaitScope(t *testing.T, f fixture) computer.Scope {
	t.Helper()
	scope := computer.Scope{EnvironmentID: f.env}
	if err := f.pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.env).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	return scope
}

func assertExecutionWait(t *testing.T, f fixture, id uuid.UUID, want string) {
	t.Helper()
	scope := executionWaitScope(t, f)
	snapshot, err := computer.Read(t.Context(), f.pool, scope, id)
	if err != nil {
		t.Fatal(err)
	}
	check := func(wait *computer.ExecutionWait) {
		t.Helper()
		got := ""
		if wait != nil {
			got = wait.Reason
		}
		if got != want {
			t.Fatalf("execution wait = %q, want %q", got, want)
		}
	}
	check(snapshot.ExecutionWait)
	list, err := computer.List(t.Context(), f.pool, scope, computer.ListPage{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if item.ID == id.String() {
			check(item.ExecutionWait)
			return
		}
	}
	t.Fatal("Computer missing from list")
}

func TestComputerExecutionWaitAdmissionAndHolds(t *testing.T) {
	f, a, id := freshComputerAllocationFixture(t)
	assertExecutionWait(t, f, id, "scheduling")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=1 WHERE id=$1`, f.env)
	assertExecutionWait(t, f, id, "environment_limit")
	if _, err := a.AllocateFreshLease(t.Context(), f.env, id, 1, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("quota did not block admission: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind,issuer_id) SELECT environment_id,$3,id,'interrupt','user',$3 FROM sessions WHERE environment_id=$1 AND computer_id=$2`, f.env, id, uuid.NewV7())
	assertExecutionWait(t, f, id, "")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_holds SET released_at=clock_timestamp() WHERE environment_id=$1`, f.env)
	assertExecutionWait(t, f, id, "environment_limit")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=8000 WHERE id=$1`, f.env)
	assertExecutionWait(t, f, id, "scheduling")
	if _, err := a.AllocateFreshLease(t.Context(), f.env, id, 1, f.worker); err != nil {
		t.Fatal(err)
	}
	assertExecutionWait(t, f, id, "") // Acquiring already has starting residency.
}

func TestComputerExecutionWaitIdleAndErrors(t *testing.T) {
	f, _, id := freshComputerAllocationFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,kind,issuer_kind) SELECT environment_id,$3,id,'process_failed','system' FROM sessions WHERE environment_id=$1 AND computer_id=$2`, f.env, id, uuid.NewV7())
	assertExecutionWait(t, f, id, "")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET status='closed',close_how='close' WHERE environment_id=$1 AND computer_id=$2`, f.env, id)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_holds SET released_at=clock_timestamp() WHERE environment_id=$1`, f.env)
	assertExecutionWait(t, f, id, "")
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET integrity_fault_at=clock_timestamp(),integrity_fault_reason='test fault' WHERE environment_id=$1 AND id=$2`, f.env, id)
	assertExecutionWait(t, f, id, "")
}

func TestComputerExecutionWaitSavingAndActiveWork(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "background_save", true: "checkpoint"}[checkpoint], func(t *testing.T) {
			f, _, prepared := freshComputerAllocationFixture(t)
			// The active fixture predates image preparation; attach the published image.
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers c SET image_spec_id=p.image_spec_id,image_id=p.image_id FROM computers p WHERE c.environment_id=$1 AND c.id=$2 AND p.environment_id=c.environment_id AND p.id=$3`, f.env, f.computer, prepared)
			if checkpoint {
				beginCapture(t, f, f.captureRequest())
			} else {
				f.finalize(t, "previous")
			}
			f.enqueue(t, "follow-up")
			want := ""
			if checkpoint {
				want = "saving"
			}
			assertExecutionWait(t, f, f.computer, want)
		})
	}
}
