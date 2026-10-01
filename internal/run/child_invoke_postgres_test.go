package run

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

// childInvokeFixture is a running Task execution, entered at its
// entrypoint, whose deployment declares the Task "test-task".
type childInvokeFixture struct {
	postgresFixture
	parent leasedRun
	fence  ExecutionFence
}

func newChildInvokeFixture(t *testing.T) childInvokeFixture {
	t.Helper()
	f := childInvokeFixture{postgresFixture: newPostgresFixture(t)}
	declareTestTask(t, f.postgresFixture)
	f.parent = f.addRun(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, f.parent.runID)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, f.parent.runID)
	f.fence = ExecutionFence{
		LeaseID: pgvalue.UUID(f.parent.leaseID), LeaseSequence: 1,
		WorkerHostID: pgvalue.UUID(f.workerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID),
		WorkerEpoch: 1, GroupClaimVersion: 1, HostClaimVersion: 1,
	}
	return f
}

// invoke is a keyed detached start of "test-task" on the parent's own
// Computer.
func (f childInvokeFixture) invoke(t *testing.T, key string) ChildInvoke {
	t.Helper()
	locators, err := LocateChildInvocation(t.Context(), f.queries, f.fence)
	if err != nil {
		t.Fatal(err)
	}
	computerID := pgvalue.MustUUIDValue(locators.ComputerID)
	task := TaskStart{
		OrgID: pgvalue.MustUUIDValue(locators.OrgID), ProjectID: pgvalue.MustUUIDValue(locators.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(locators.EnvironmentID), TaskDeclaredID: "test-task",
		PayloadPresent: true, Payload: json.RawMessage(`{"imageId":"child"}`), ComputerID: computerID,
		Metadata: json.RawMessage(`{}`), Tags: []string{},
	}
	return ChildInvoke{
		Fence: f.fence, Method: "start", SourceComputerID: computerID, Task: task, IdempotencyKey: key,
		Fingerprint: idempotency.TaskChildInvokeFingerprint{
			Method: "start", PayloadPresent: true, Payload: task.Payload,
			Computer: json.RawMessage(`{"id":"` + computerID.String() + `"}`),
			Metadata: task.Metadata, Tags: task.Tags,
		},
	}
}

func TestInvokeChildStartsAndReplaysOneDetachedChild(t *testing.T) {
	f := newChildInvokeFixture(t)
	invoke := f.invoke(t, "child-once")
	started, err := InvokeChild(t.Context(), f.pool, invoke)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := InvokeChild(t.Context(), f.pool, invoke)
	if err != nil {
		t.Fatal(err)
	}
	if started.Replayed || started.Call != nil || !replayed.Replayed || replayed.RunID != started.RunID {
		t.Fatalf("started=%+v replayed=%+v", started, replayed)
	}
	var parentID uuid.UUID
	var cause string
	var ownsLifecycle bool
	var resolutions int
	if err := f.pool.QueryRow(t.Context(), `SELECT parent_run_id, cause_kind, parent_owns_lifecycle,
		(SELECT count(*) FROM run_attempts WHERE run_id=r.id) FROM runs r WHERE id=$1`, started.RunID).Scan(&parentID, &cause, &ownsLifecycle, &resolutions); err != nil {
		t.Fatal(err)
	}
	if parentID != f.parent.runID || cause != "child" || ownsLifecycle || resolutions != 1 {
		t.Fatalf("parent=%s cause=%s owns=%v attempts=%d", parentID, cause, ownsLifecycle, resolutions)
	}
}

func TestInvokeChildRejectsStaleAndUnadmittedInvocations(t *testing.T) {
	f := newChildInvokeFixture(t)
	for name, test := range map[string]struct {
		prepare func(*ChildInvoke)
		want    error
	}{
		"stale lease":       {func(invoke *ChildInvoke) { invoke.Fence.LeaseSequence++ }, ErrChildInvokeStale},
		"other source":      {func(invoke *ChildInvoke) { invoke.Task.OrgID = uuid.NewV7() }, ErrChildInvokeSourceScope},
		"actor cursor":      {func(invoke *ChildInvoke) { invoke.Cursor = new(int64) }, ErrChildInvokeStale},
		"undeclared task":   {func(invoke *ChildInvoke) { invoke.Task.TaskDeclaredID = "missing-task" }, ErrTaskNotDeployed},
		"payload presence":  {func(invoke *ChildInvoke) { invoke.Task.PayloadPresent, invoke.Task.Payload = false, nil }, ErrTaskPayloadPresenceInvalid},
		"unknown computer":  {func(invoke *ChildInvoke) { invoke.Task.ComputerID = uuid.NewV7() }, ErrTaskComputerNotFound},
		"turn on task work": {func(invoke *ChildInvoke) { invoke.TurnID = pgvalue.UUID(uuid.NewV7()) }, ErrTurnScope},
	} {
		t.Run(name, func(t *testing.T) {
			invoke := f.invoke(t, "")
			test.prepare(&invoke)
			if _, err := InvokeChild(t.Context(), f.pool, invoke); !errors.Is(err, test.want) {
				t.Fatalf("invoke error = %v, want %v", err, test.want)
			}
		})
	}
	var children int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM runs WHERE parent_run_id=$1`, f.parent.runID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 0 {
		t.Fatalf("rejected invocations created %d children", children)
	}
}
