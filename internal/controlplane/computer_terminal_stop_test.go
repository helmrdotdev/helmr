package controlplane

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestCancelledParkedComputerBecomesDeletableAfterLifecycleReconciliation(t *testing.T) {
	f := sessiontest.New(t)
	// A retained checkpoint has already allowed source physical cleanup without
	// terminating its logical process. The Agent tests cover that publication path.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='owned physical closure' WHERE computer_id=$1`, f.Computer)
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", IdempotencyKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	scope := computer.Scope{EnvironmentID: f.Environment}
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	deletion := computer.Deletion{Principal: f.Principal(t), Scope: scope, ComputerID: f.Computer, IdempotencyKey: "delete"}
	if _, err := computer.Delete(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, deletion); !errors.Is(err, computer.ErrBusy) {
		t.Fatalf("unsettled process deletion: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- session.RunSessionLifecycle(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("lifecycle exit: %v", err)
		}
	}()
	limit := time.Now().Add(5 * time.Second)
	for {
		result, err := computer.Delete(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, deletion)
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
	replay, err := computer.Delete(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, deletion)
	if err != nil || !replay.Replayed {
		t.Fatalf("delete replay: %+v %v", replay, err)
	}
}
