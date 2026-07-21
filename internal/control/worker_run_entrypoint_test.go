package control

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEnterRunEntrypointCommitsOnceAndReplaysTheSameFence(t *testing.T) {
	worker, locators, authority, receipt := validRunEntrypointFixture(t)
	store := &runLeaseClaimStore{
		authority:  authority,
		entrypoint: locators,
		enteredAt: pgtype.Timestamptz{
			Time:  time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC),
			Valid: true,
		},
	}
	request := api.WorkerRunEntrypointRequest{
		Lease:                receipt,
		EntrypointKind:       authority.run.EntrypointKind,
		EntrypointDeclaredID: authority.run.EntrypointDeclaredID,
	}

	if err := enterRunEntrypoint(context.Background(), store, nil, worker, authority.runLease.ID, request); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"entrypoint_locators", "run", "workspace", "attempt",
		"worker_group", "worker", "network_slot", "runtime",
		"entrypoint_lease", "workspace_mount", "workspace_lease",
		"mark_entrypoint", "commit",
	}
	if !slices.Equal(store.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", store.calls, wantCalls)
	}
	if !store.authority.attempt.EntrypointEnteredAt.Valid {
		t.Fatal("entrypoint was not recorded")
	}
	enteredAt := store.authority.attempt.EntrypointEnteredAt.Time

	store.calls = nil
	if err := enterRunEntrypoint(context.Background(), store, nil, worker, authority.runLease.ID, request); err != nil {
		t.Fatal(err)
	}
	if store.entrypointMarks != 1 {
		t.Fatalf("entrypoint marks = %d, want 1", store.entrypointMarks)
	}
	if !store.authority.attempt.EntrypointEnteredAt.Time.Equal(enteredAt) {
		t.Fatalf("replay changed entrypoint timestamp: got %v want %v", store.authority.attempt.EntrypointEnteredAt.Time, enteredAt)
	}
	if slices.Contains(store.calls, "mark_entrypoint") ||
		len(store.calls) == 0 ||
		store.calls[len(store.calls)-1] != "commit" {
		t.Fatalf("replay calls = %v", store.calls)
	}
}

func TestEnterRunEntrypointRollsBackMismatchedReceiptAndIdentity(t *testing.T) {
	for name, change := range map[string]func(*api.WorkerRunEntrypointRequest){
		"receipt": func(request *api.WorkerRunEntrypointRequest) {
			request.Lease.WriterGeneration++
		},
		"identity": func(request *api.WorkerRunEntrypointRequest) {
			request.EntrypointDeclaredID = "different"
		},
	} {
		t.Run(name, func(t *testing.T) {
			worker, locators, authority, receipt := validRunEntrypointFixture(t)
			store := &runLeaseClaimStore{authority: authority, entrypoint: locators}
			request := api.WorkerRunEntrypointRequest{
				Lease:                receipt,
				EntrypointKind:       authority.run.EntrypointKind,
				EntrypointDeclaredID: authority.run.EntrypointDeclaredID,
			}
			change(&request)

			err := enterRunEntrypoint(context.Background(), store, nil, worker, authority.runLease.ID, request)
			if !errors.Is(err, errStaleRunLeaseClaim) {
				t.Fatalf("error = %v, want stale claim", err)
			}
			if store.entrypointMarks != 0 ||
				!slices.Contains(store.calls, "rollback") ||
				slices.Contains(store.calls, "mark_entrypoint") {
				t.Fatalf("calls = %v marks = %d", store.calls, store.entrypointMarks)
			}
		})
	}
}

func TestEnterRunEntrypointRejectsMountedBaseOutsideAttempt(t *testing.T) {
	worker, locators, authority, receipt := validRunEntrypointFixture(t)
	differentBase := pgvalue.UUID(uuid.New())
	authority.workspaceLease.BaseVersionID = differentBase
	authority.workspaceMount.MaterializedVersionID = differentBase
	store := &runLeaseClaimStore{authority: authority, entrypoint: locators}

	err := enterRunEntrypoint(context.Background(), store, nil, worker, authority.runLease.ID, api.WorkerRunEntrypointRequest{
		Lease:                receipt,
		EntrypointKind:       authority.run.EntrypointKind,
		EntrypointDeclaredID: authority.run.EntrypointDeclaredID,
	})
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if store.entrypointMarks != 0 ||
		!slices.Contains(store.calls, "rollback") ||
		slices.Contains(store.calls, "mark_entrypoint") {
		t.Fatalf("calls = %v marks = %d", store.calls, store.entrypointMarks)
	}
}

func (s *runLeaseClaimStore) GetRunEntrypointLocators(
	context.Context,
	db.GetRunEntrypointLocatorsParams,
) (db.GetRunEntrypointLocatorsRow, error) {
	s.calls = append(s.calls, "entrypoint_locators")
	return s.entrypoint, nil
}

func (s *runLeaseClaimStore) LockRunEntrypointLease(
	context.Context,
	db.LockRunEntrypointLeaseParams,
) (db.RunLease, error) {
	s.calls = append(s.calls, "entrypoint_lease")
	return s.authority.runLease, nil
}

func (s *runLeaseClaimStore) MarkRunEntrypointEntered(
	context.Context,
	db.MarkRunEntrypointEnteredParams,
) (db.RunAttempt, error) {
	s.calls = append(s.calls, "mark_entrypoint")
	s.entrypointMarks++
	s.authority.attempt.EntrypointEnteredAt = s.enteredAt
	return s.authority.attempt, nil
}

func validRunEntrypointFixture(
	t *testing.T,
) (workerActor, db.GetRunEntrypointLocatorsRow, runLeaseClaimAuthority, api.WorkerRunLeaseReceipt) {
	t.Helper()
	worker, claimLocators, authority := validRunLeaseClaimFixture()
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	authority.run.EntrypointDeclaredID = "compile"
	authority.run.Status = db.RunStatusRunning
	authority.run.StartedAt = pgtype.Timestamptz{Time: now, Valid: true}
	authority.run.ActiveStartedAt = pgtype.Timestamptz{Time: now, Valid: true}
	authority.run.MaxActiveDurationMs = int64(time.Hour / time.Millisecond)
	authority.runLease.State = db.RunLeaseStateRunning
	authority.runLease.StartedAt = pgtype.Timestamptz{Time: now, Valid: true}
	authority.runLease.StartDeadlineAt = pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true}
	authority.runLease.ExpiresAt = pgtype.Timestamptz{Time: now.Add(5 * time.Minute), Valid: true}
	authority.workspaceMount.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.WorkspaceID = authority.workspace.ID
	authority.workspaceLease.WorkspaceMountID = authority.workspaceMount.ID

	receipt, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
		run:            authority.run,
		attempt:        authority.attempt,
		runtime:        authority.runtime,
		networkSlot:    authority.networkSlot,
		runLease:       authority.runLease,
		workspace:      authority.workspace,
		workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker, db.GetRunEntrypointLocatorsRow{
		OrgID:                 claimLocators.OrgID,
		ProjectID:             claimLocators.ProjectID,
		EnvironmentID:         claimLocators.EnvironmentID,
		RunID:                 claimLocators.RunID,
		WorkspaceID:           claimLocators.WorkspaceID,
		AttemptNumber:         claimLocators.AttemptNumber,
		RegionID:              claimLocators.RegionID,
		RuntimeInstanceID:     claimLocators.RuntimeInstanceID,
		NetworkSlotID:         claimLocators.NetworkSlotID,
		NetworkSlotGeneration: claimLocators.NetworkSlotGeneration,
		WorkspaceLeaseID:      claimLocators.WorkspaceLeaseID,
		WorkspaceMountID:      claimLocators.WorkspaceMountID,
	}, authority, receipt
}
