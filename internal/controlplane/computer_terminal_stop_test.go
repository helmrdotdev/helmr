package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestCancelledParkedComputerBecomesDeletableAfterLifecycleReconciliation(t *testing.T) {
	f := agenttest.New(t)
	// A retained checkpoint has already allowed source physical cleanup without
	// terminating its logical process. The Agent tests cover that publication path.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='owned physical closure' WHERE computer_id=$1`, f.Computer)
	if _, err := agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", RetryKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	scope := computer.Scope{EnvironmentID: f.Environment}
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	deletion := computer.Deletion{Scope: scope, ComputerID: f.Computer, IdempotencyKey: "delete"}
	if _, err := computer.Delete(t.Context(), f.Pool, deletion); !errors.Is(err, computer.ErrBusy) {
		t.Fatalf("unsettled process deletion: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- agent.RunSessionLifecycle(ctx, f.Pool, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("lifecycle exit: %v", err)
		}
	}()
	limit := time.Now().Add(5 * time.Second)
	for {
		result, err := computer.Delete(t.Context(), f.Pool, deletion)
		if err == nil {
			if result.ComputerID != f.Computer || result.Replayed {
				t.Fatalf("unexpected deletion receipt: %+v", result)
			}
			break
		}
		if !errors.Is(err, computer.ErrBusy) || time.Now().After(limit) {
			t.Fatalf("terminal parked Computer deletion never converged: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	replay, err := computer.Delete(t.Context(), f.Pool, deletion)
	if err != nil || !replay.Replayed {
		t.Fatalf("delete replay: %+v %v", replay, err)
	}
}
