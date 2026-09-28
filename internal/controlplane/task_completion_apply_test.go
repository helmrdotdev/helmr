package controlplane

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTaskCompletionReplayUsesOnlyTerminalReceipt(t *testing.T) {
	workerID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	leaseID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	worker := workerActor{WorkerHostID: workerID, WorkerGroupID: controlplaneTestWorkerGroupID}
	request := workerapi.CompleteTaskRequest{Lease: workerapi.RunLeaseFence{
		ID:            leaseID.String(),
		LeaseSequence: 7,
	}}
	completion := parsedTaskCompletion{
		lease:       parsedRunLeaseFence{leaseID: leaseID},
		fingerprint: "sha256:receipt",
	}
	store := &taskCompletionReplayFixture{fingerprint: pgvalue.Text(completion.fingerprint)}

	replayed, err := taskCompletionWasReplayed(context.Background(), store, worker, request, completion)
	if err != nil || !replayed {
		t.Fatalf("replay = %t, %v", replayed, err)
	}
	if store.last.RunLeaseID != pgvalue.UUID(leaseID) ||
		store.last.LeaseSequence != 7 ||
		store.last.WorkerGroupID != pgvalue.UUID(worker.WorkerGroupID) ||
		store.last.WorkerHostID != pgvalue.UUID(workerID) {
		t.Fatalf("unexpected replay selector: %+v", store.last)
	}
}

func TestTaskCompletionReplayRejectsChangedFingerprint(t *testing.T) {
	store := &taskCompletionReplayFixture{fingerprint: pgvalue.Text("sha256:committed")}
	_, err := taskCompletionWasReplayed(
		context.Background(),
		store,
		workerActor{},
		workerapi.CompleteTaskRequest{},
		parsedTaskCompletion{fingerprint: "sha256:changed"},
	)
	if !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion", err)
	}
}

func TestTaskCompletionFailurePointPreservesStaleIdentity(t *testing.T) {
	err := staleAuthority(staleAuthorityTaskCompletion, "execution", errStaleTaskCompletion)
	if !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion identity", err)
	}
	point, ok := staleAuthorityPointOf(fmt.Errorf("outer: %w", err))
	if !ok || point != "execution" {
		t.Fatalf("failure point = %q, %t", point, ok)
	}
	if got := staleAuthority(staleAuthorityTaskCompletion, "outer", err); got != err {
		t.Fatal("outer failure point replaced the owning point")
	}
	plain := errors.New("storage unavailable")
	if got := staleAuthority(staleAuthorityTaskCompletion, "execution", plain); got != plain {
		t.Fatal("non-stale error was wrapped")
	}
}

func TestTaskCompletionReplayAfterAmbiguousError(t *testing.T) {
	operationErr := errors.New("commit result is unknown")
	completion := parsedTaskCompletion{fingerprint: "sha256:receipt"}
	if err := taskCompletionReplayAfterError(
		context.Background(),
		&taskCompletionReplayFixture{fingerprint: pgvalue.Text(completion.fingerprint)},
		workerActor{},
		workerapi.CompleteTaskRequest{},
		completion,
		operationErr,
	); err != nil {
		t.Fatalf("confirmed replay returned %v", err)
	}
	if err := taskCompletionReplayAfterError(
		context.Background(),
		&taskCompletionReplayFixture{err: pgx.ErrNoRows},
		workerActor{},
		workerapi.CompleteTaskRequest{},
		completion,
		operationErr,
	); !errors.Is(err, operationErr) {
		t.Fatalf("uncommitted error = %v, want original", err)
	}
}

type taskCompletionReplayFixture struct {
	fingerprint pgtype.Text
	err         error
	last        db.GetTaskCompletionReplayParams
}

func (fixture *taskCompletionReplayFixture) GetTaskCompletionReplay(_ context.Context, params db.GetTaskCompletionReplayParams) (pgtype.Text, error) {
	fixture.last = params
	return fixture.fingerprint, fixture.err
}
