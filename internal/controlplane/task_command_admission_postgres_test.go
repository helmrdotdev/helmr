package controlplane

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
	"uuid"
)

func TestTaskAndCommandAdmissionShareSecretLockOrder(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	locked := make(chan int, 1)
	release := make(chan struct{})
	commandDone := make(chan error, 1)
	go func() {
		_, err := f.server.admitComputerCommand(ctx, computerCommandRequest{
			OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, ComputerID: f.computerIDs[0],
			Creator: computerCommandCreator{SubjectType: string(auth.ActorKindAPIKey), SubjectID: uuid.NewV7().String()}, Command: []string{"true"}, IdempotencyKey: "command-secret-order",
			Authorize: func(ctx context.Context, tx pgx.Tx) error {
				if _, err := db.New(tx).LockComputerSecretsForAdmission(ctx, pgvalue.UUID(f.computerIDs[0])); err != nil {
					return err
				}
				var pid int
				if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					return err
				}
				locked <- pid
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
		commandDone <- err
	}()
	var pid int
	select {
	case pid = <-locked:
	case err := <-commandDone:
		t.Fatalf("command failed before lock: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	taskDone := make(chan error, 1)
	go func() {
		_, err := f.server.startTask(ctx, taskStartRequest{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID, TaskDeclaredID: "resize-image", PayloadPresent: true, Payload: json.RawMessage(`{"imageId":"lock-order"}`), ComputerID: f.computerIDs[0], IdempotencyKey: "task-secret-order"})
		taskDone <- err
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
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
