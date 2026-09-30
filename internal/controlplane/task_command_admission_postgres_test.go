package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/command"
)

// Command creation and task start lock a Computer's Secrets before the
// Computer: a Command that holds the Secrets while it waits for the Computer
// makes a task start on that Computer wait for the Command, not the other
// way round.
func TestTaskAndCommandAdmissionShareSecretLockOrder(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	var holderPID int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid() FROM computers WHERE id=$1 FOR UPDATE`, f.computerIDs[0]).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	commandDone := make(chan error, 1)
	go func() {
		_, err := command.Create(ctx, f.pool, command.CreateRequest{
			OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, ComputerID: f.computerIDs[0],
			Creator: command.Creator{SubjectType: string(auth.PrincipalKindAPIKey), SubjectID: uuid.NewV7().String()}, Argv: []string{"true"}, IdempotencyKey: "command-secret-order",
		})
		commandDone <- err
	}()
	commandPID := waitForBlockedBackend(ctx, t, f, holderPID, commandDone)
	taskDone := make(chan error, 1)
	go func() {
		_, err := f.server.startTask(ctx, taskStartRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, TaskDeclaredID: "resize-image", PayloadPresent: true, Payload: json.RawMessage(`{"imageId":"lock-order"}`), ComputerID: f.computerIDs[0], IdempotencyKey: "task-secret-order"})
		taskDone <- err
	}()
	taskPID := waitForBlockedBackend(ctx, t, f, commandPID, taskDone)
	// The task waits for the Command's transaction while it holds the tuple
	// lock of a Secret row; it holds and waits for nothing on the Computer.
	var waitsForCommand, holdsSecretTuple, touchesComputer bool
	if err := f.pool.QueryRow(ctx, `WITH command AS (
 SELECT transactionid FROM pg_locks WHERE pid=$2 AND locktype='transactionid' AND granted AND mode='ExclusiveLock'
), task AS (SELECT * FROM pg_locks WHERE pid=$1)
SELECT
 EXISTS(SELECT 1 FROM task WHERE locktype='transactionid' AND NOT granted AND transactionid IN (SELECT transactionid FROM command)),
 EXISTS(SELECT 1 FROM task WHERE locktype='tuple' AND relation='secrets'::regclass AND granted),
 EXISTS(SELECT 1 FROM task WHERE (locktype='tuple' AND relation='computers'::regclass)
  OR (locktype='transactionid' AND NOT granted AND transactionid NOT IN (SELECT transactionid FROM command)))`,
		taskPID, commandPID).Scan(&waitsForCommand, &holdsSecretTuple, &touchesComputer); err != nil {
		t.Fatal(err)
	}
	if !waitsForCommand || !holdsSecretTuple || touchesComputer {
		t.Fatalf("task start waits for Command=%v holds Secret tuple=%v touches Computer=%v", waitsForCommand, holdsSecretTuple, touchesComputer)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, done := range []<-chan error{commandDone, taskDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// waitForBlockedBackend returns the backend that waits for a lock blocker
// holds, failing if the waiting operation finishes first.
func waitForBlockedBackend(ctx context.Context, t *testing.T, f actorStartPostgresFixture, blocker int, done <-chan error) int {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int
		err := f.pool.QueryRow(ctx, `SELECT COALESCE(max(pid),0) FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))`, blocker).Scan(&pid)
		if err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			return pid
		}
		select {
		case err := <-done:
			t.Fatalf("operation finished before it waited on %d: %v", blocker, err)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
