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

func TestCancelledParkedComputerDeletionSettlesProcessThroughLifecycle(t *testing.T) {
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
	deletion := computer.Deletion{Principal: f.Principal(t), Scope: scope, ComputerID: f.Computer, IdempotencyKey: "delete", CancelSessions: session.ComputerSessionCancellation(auditpolicy.Policy{RetentionSeconds: 3600})}
	// The closed Session's parked process does not delay deletion; it settles
	// through the deleting Computer's lifecycle.
	result, err := computer.Delete(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, deletion)
	if err != nil || result.ComputerID != f.Computer || result.Replayed {
		t.Fatalf("unexpected deletion receipt: %+v %v", result, err)
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
		var stopped bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT bool_and(fenced_at IS NOT NULL) FROM session_processes WHERE session_id=$1`, f.Session).Scan(&stopped); err != nil {
			t.Fatal(err)
		}
		if stopped {
			break
		}
		if time.Now().After(limit) {
			t.Fatal("terminal parked process never settled after deletion")
		}
		time.Sleep(time.Millisecond)
	}
	replay, err := computer.Delete(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, deletion)
	if err != nil || !replay.Replayed {
		t.Fatalf("delete replay: %+v %v", replay, err)
	}
}
