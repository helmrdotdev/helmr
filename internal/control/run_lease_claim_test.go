package control

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestClaimRunLeaseLocksSecretsBeforeExecutionAuthority(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	secretRow, secretVersion := validRunLeaseClaimSecretFixture(locators)
	store := &runLeaseClaimStore{
		authority: authority,
		locators:  locators,
		secretRows: []db.LockAttemptSecretDeliveryRow{
			secretRow,
		},
		secretVersion: secretVersion,
	}
	server := &Server{db: store}

	claimed, envelopes, err := server.claimRunLease(
		context.Background(),
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(envelopes) != 1 ||
		envelopes[0].Secret.ID != secretRow.Secret.ID ||
		envelopes[0].Version.ID != secretVersion.ID {
		t.Fatalf("Secret envelopes = %+v", envelopes)
	}
	if claimed.runLease.State != db.RunLeaseStateStarting {
		t.Fatalf("lease state = %q, want starting", claimed.runLease.State)
	}
	if claimed.mode != runLeaseClaimFresh {
		t.Fatalf("claim mode = %q, want fresh", claimed.mode)
	}
	if !slices.Equal(store.calls, []string{
		"secret_locators", "secrets", "secret_version", "locators",
		"run", "workspace", "attempt",
		"worker_group", "worker", "network_slot", "runtime", "run_lease",
		"workspace_mount", "workspace_lease", "mark_starting", "commit",
	}) {
		t.Fatalf("claim order = %v", store.calls)
	}
}

func TestClaimRunLeaseRollsBackWhenSecretLocatorsChange(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	secretLocators := db.GetRunLeaseSecretDeliveryLocatorsRow{
		EnvironmentID: locators.EnvironmentID,
		RunID:         locators.RunID,
		WorkspaceID:   pgvalue.UUID(uuid.New()),
		AttemptNumber: locators.AttemptNumber,
	}
	store := &runLeaseClaimStore{
		authority:      authority,
		locators:       locators,
		secretLocators: &secretLocators,
	}
	server := &Server{db: store}

	_, _, err := server.claimRunLease(
		context.Background(),
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if !slices.Equal(store.calls, []string{
		"secret_locators", "secrets", "locators", "rollback",
	}) {
		t.Fatalf("claim order = %v", store.calls)
	}
	if store.authority.runLease.State != db.RunLeaseStateAssigned {
		t.Fatalf("lease state = %q, want assigned", store.authority.runLease.State)
	}
}

func TestClaimFreshTaskRunLeaseInTxLocksCanonicalOrderAndTransitionsOnce(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	store := &runLeaseClaimStore{authority: authority}

	claimed, err := claimFreshTaskRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{
		"run",
		"workspace",
		"attempt",
		"worker_group",
		"worker",
		"network_slot",
		"runtime",
		"run_lease",
		"workspace_mount",
		"workspace_lease",
		"mark_starting",
	}
	if !slices.Equal(store.calls, wantOrder) {
		t.Fatalf("lock order = %v, want %v", store.calls, wantOrder)
	}
	if claimed.runLease.State != db.RunLeaseStateStarting {
		t.Fatalf("claim state = %s", claimed.runLease.State)
	}

	store.calls = nil
	store.authority.runLease = claimed.runLease
	replayed, err := claimFreshTaskRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(store.calls, "mark_starting") {
		t.Fatalf("replay rewrote claim: %v", store.calls)
	}
	if replayed.runLease.State != db.RunLeaseStateStarting {
		t.Fatalf("replay state = %s", replayed.runLease.State)
	}
}

func TestClaimFreshTaskRunLeaseInTxRejectsAssignedWorkFromDrainingWorker(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	authority.worker.State = db.WorkerInstanceStateDraining
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimFreshTaskRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if slices.Contains(store.calls, "mark_starting") {
		t.Fatalf("draining worker transitioned assigned work: %v", store.calls)
	}
}

func TestClaimFreshTaskRunLeaseInTxRejectsMismatchedMountGeneration(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	authority.workspaceLease.MountFencingGeneration++
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimFreshTaskRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if slices.Contains(store.calls, "mark_starting") {
		t.Fatalf("mismatched authority transitioned lease: %v", store.calls)
	}
}

func TestClaimFreshTaskRunLeaseInTxRejectsMismatchedAttemptBase(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	authority.workspaceLease.BaseVersionID = pgvalue.UUID(uuid.New())
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimFreshTaskRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if slices.Contains(store.calls, "mark_starting") {
		t.Fatalf("mismatched base transitioned lease: %v", store.calls)
	}
}

func TestClaimFreshTaskRunLeaseInTxRejectsAttemptBaseThatDiffersFromRun(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	otherVersionID := pgvalue.UUID(uuid.New())
	authority.attempt.BaseWorkspaceVersionID = otherVersionID
	authority.workspaceMount.MaterializedVersionID = otherVersionID
	authority.workspaceLease.BaseVersionID = otherVersionID
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimFreshTaskRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if !slices.Equal(store.calls, []string{"run", "workspace", "attempt"}) {
		t.Fatalf("mismatched Run base progressed claim: %v", store.calls)
	}
}

func TestClaimFreshTaskRunLeaseInTxRejectsActorAuthority(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	actorID := pgvalue.UUID(uuid.New())
	authority.run.EntrypointKind = "actor"
	authority.run.ActorID = actorID
	authority.attempt.EntrypointKind = "actor"
	authority.workspace.OwnerRunID = pgtype.UUID{}
	authority.workspace.OwnerActorID = actorID
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimFreshTaskRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if !slices.Equal(store.calls, []string{"run", "workspace"}) {
		t.Fatalf("actor authority progressed through task claim: %v", store.calls)
	}
}

func TestClaimFreshTaskRunLeaseInTxRejectsStaleGroupGeneration(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	authority.workerGroup.ClaimVersion++
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimFreshTaskRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if !slices.Equal(store.calls, []string{"run", "workspace", "attempt", "worker_group"}) {
		t.Fatalf("stale group generation progressed claim: %v", store.calls)
	}
}

func TestFreshRunLeaseClaimsRejectRestoreLocator(t *testing.T) {
	worker, locators, authority := validRunLeaseClaimFixture()
	locators.RunWaitID = pgvalue.UUID(uuid.New())
	store := &runLeaseClaimStore{authority: authority}
	if _, err := claimFreshTaskRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("Task error = %v, want stale claim", err)
	}

	worker, locators, authority = validActorRunLeaseClaimFixture()
	locators.RunWaitID = pgvalue.UUID(uuid.New())
	store = &runLeaseClaimStore{authority: authority}
	if _, err := claimActorRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("Actor error = %v, want stale claim", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("restore locator acquired fresh-claim locks: %v", store.calls)
	}
}

func TestClaimActorRunLeaseInTxLocksActorBeforeRun(t *testing.T) {
	worker, locators, authority := validActorRunLeaseClaimFixture()
	store := &runLeaseClaimStore{authority: authority}

	claimed, err := claimActorRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{
		"actor",
		"run",
		"workspace",
		"attempt",
		"worker_group",
		"worker",
		"network_slot",
		"runtime",
		"run_lease",
		"workspace_mount",
		"workspace_lease",
		"mark_starting",
	}
	if !slices.Equal(store.calls, wantOrder) {
		t.Fatalf("lock order = %v, want %v", store.calls, wantOrder)
	}
	if claimed.runLease.State != db.RunLeaseStateStarting {
		t.Fatalf("claim state = %s", claimed.runLease.State)
	}
}

func TestClaimActorRunLeaseInTxRejectsStaleActorGeneration(t *testing.T) {
	worker, locators, authority := validActorRunLeaseClaimFixture()
	authority.actor.RunGeneration++
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimActorRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if !slices.Equal(store.calls, []string{"actor"}) {
		t.Fatalf("stale Actor generation progressed claim: %v", store.calls)
	}
}

func TestClaimActorRunLeaseInTxAcceptsRetryAttemptFrontier(t *testing.T) {
	worker, locators, authority := validActorRunLeaseClaimFixture()
	retryVersionID := pgvalue.UUID(uuid.New())
	locators.AttemptNumber = 2
	authority.run.CurrentAttemptNumber = 2
	authority.attempt.Number = 2
	authority.attempt.ActorStartInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	authority.attempt.BaseWorkspaceVersionID = retryVersionID
	authority.actor.CommittedInputSequence = 2
	authority.workspace.HeadVersionID = retryVersionID
	authority.runLease.AttemptNumber = 2
	authority.workspaceMount.MaterializedVersionID = retryVersionID
	authority.workspaceLease.BaseVersionID = retryVersionID
	store := &runLeaseClaimStore{authority: authority}

	if _, err := claimActorRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	); err != nil {
		t.Fatal(err)
	}
}

func TestClaimActorRunLeaseInTxRejectsRetryCursorMismatch(t *testing.T) {
	worker, locators, authority := validActorRunLeaseClaimFixture()
	locators.AttemptNumber = 2
	authority.run.CurrentAttemptNumber = 2
	authority.attempt.Number = 2
	authority.attempt.ActorStartInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	authority.runLease.AttemptNumber = 2
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimActorRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
}

func TestClaimActorRunLeaseInTxRejectsRetryBaseThatIsNotWorkspaceHead(t *testing.T) {
	worker, locators, authority := validActorRunLeaseClaimFixture()
	retryVersionID := pgvalue.UUID(uuid.New())
	locators.AttemptNumber = 2
	authority.run.CurrentAttemptNumber = 2
	authority.attempt.Number = 2
	authority.attempt.ActorStartInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	authority.attempt.BaseWorkspaceVersionID = retryVersionID
	authority.actor.CommittedInputSequence = 2
	authority.runLease.AttemptNumber = 2
	authority.workspaceMount.MaterializedVersionID = retryVersionID
	authority.workspaceLease.BaseVersionID = retryVersionID
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimActorRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxUsesCheckpointBase(t *testing.T) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(false)
	store := &runLeaseClaimStore{authority: authority}

	claimed, err := claimCheckpointRestoreRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{
		"run",
		"workspace",
		"attempt",
		"worker_group",
		"worker",
		"network_slot",
		"runtime",
		"run_lease",
		"workspace_mount",
		"workspace_lease",
		"mark_starting",
		"run_wait",
		"checkpoint",
		"checkpoint_source",
	}
	if !slices.Equal(store.calls, wantOrder) {
		t.Fatalf("lock order = %v, want %v", store.calls, wantOrder)
	}
	if claimed.workspaceLease.BaseVersionID == claimed.attempt.BaseWorkspaceVersionID {
		t.Fatal("restore reused Attempt base instead of Checkpoint private version")
	}
	if claimed.workspaceLease.BaseVersionID != claimed.checkpoint.PrivateWorkspaceVersionID {
		t.Fatal("restore Workspace base does not match Checkpoint private version")
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxLocksActorBeforeRun(t *testing.T) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(true)
	store := &runLeaseClaimStore{authority: authority}

	if _, err := claimCheckpointRestoreRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) < 2 || store.calls[0] != "actor" || store.calls[1] != "run" {
		t.Fatalf("lock order = %v, want Actor before Run", store.calls)
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxRejectsHandoffResume(t *testing.T) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(false)
	locators.HandoffResumeCheckpointID = pgvalue.UUID(uuid.New())
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimCheckpointRestoreRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("handoff restore acquired locks: %v", store.calls)
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxRequiresEnteredAttempt(t *testing.T) {
	for _, actor := range []bool{false, true} {
		worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(actor)
		authority.attempt.EntrypointEnteredAt = pgtype.Timestamptz{}
		store := &runLeaseClaimStore{authority: authority}

		_, err := claimCheckpointRestoreRunLeaseInTx(
			context.Background(),
			store,
			worker,
			authority.runLease.ID,
			authority.runLease.LeaseSequence,
			locators,
		)
		if !errors.Is(err, errStaleRunLeaseClaim) {
			t.Fatalf("actor=%t error = %v, want stale claim", actor, err)
		}
		if slices.Contains(store.calls, "worker_group") {
			t.Fatalf("actor=%t unentered Attempt reached physical claim: %v", actor, store.calls)
		}
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxRejectsChangedResumeVersion(t *testing.T) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(false)
	authority.runWait.ResumeRequestVersion++
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimCheckpointRestoreRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if !slices.Contains(store.calls, "run_wait") || slices.Contains(store.calls, "checkpoint") {
		t.Fatalf("changed resume version calls = %v", store.calls)
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxRejectsActorCursorBehindCommit(t *testing.T) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(true)
	authority.actor.CommittedInputSequence = 2
	authority.checkpoint.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 1, Valid: true}
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimCheckpointRestoreRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxAcceptsCommittedActorTurns(t *testing.T) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(true)
	authority.actor.CommittedInputSequence = 2
	authority.checkpoint.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	store := &runLeaseClaimStore{authority: authority}

	if _, err := claimCheckpointRestoreRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	); err != nil {
		t.Fatal(err)
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxRejectsPhysicalProfileChange(t *testing.T) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(false)
	authority.sourceRunLease.RuntimeIdentityID = "different-runtime"
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimCheckpointRestoreRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
}

func TestClaimSameWorkspaceChildRunLeaseInTxLocksParentBeforeChild(t *testing.T) {
	for _, actorParent := range []bool{false, true} {
		worker, locators, authority := validSameWorkspaceChildRunLeaseClaimFixture(actorParent)
		store := &runLeaseClaimStore{authority: authority}

		if _, err := claimSameWorkspaceChildRunLeaseInTx(
			context.Background(),
			store,
			worker,
			authority.runLease.ID,
			authority.runLease.LeaseSequence,
			locators,
		); err != nil {
			t.Fatalf("actorParent=%t: %v", actorParent, err)
		}
		wantOrder := []string{
			"parent_run",
			"run",
			"workspace",
			"parent_attempt",
			"attempt",
			"worker_group",
			"worker",
			"network_slot",
			"runtime",
			"run_lease",
			"workspace_mount",
			"workspace_lease",
			"mark_starting",
			"handoff_wait",
			"checkpoint",
			"checkpoint_source",
		}
		if actorParent {
			wantOrder = append([]string{"actor"}, wantOrder...)
		}
		if !slices.Equal(store.calls, wantOrder) {
			t.Fatalf("actorParent=%t lock order = %v, want %v", actorParent, store.calls, wantOrder)
		}
	}
}

func TestClaimSameWorkspaceChildRunLeaseInTxRejectsDifferentRuntime(t *testing.T) {
	worker, locators, authority := validSameWorkspaceChildRunLeaseClaimFixture(false)
	authority.runWait.HandoffRuntimeInstanceID = pgvalue.UUID(uuid.New())
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimSameWorkspaceChildRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if slices.Contains(store.calls, "checkpoint") {
		t.Fatalf("mismatched handoff reached checkpoint: %v", store.calls)
	}
}

func TestClaimSameWorkspaceChildRunLeaseInTxRejectsChildAsWorkspaceOwner(t *testing.T) {
	worker, locators, authority := validSameWorkspaceChildRunLeaseClaimFixture(false)
	authority.workspace.OwnerRunID = authority.run.ID
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimSameWorkspaceChildRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
}

func TestClaimSameWorkspaceChildRunLeaseInTxRejectsChildRestoreLocator(t *testing.T) {
	worker, locators, authority := validSameWorkspaceChildRunLeaseClaimFixture(false)
	locators.RunWaitID = pgvalue.UUID(uuid.New())
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimSameWorkspaceChildRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("child restore locator acquired fresh-child locks: %v", store.calls)
	}
}

func TestClaimSameWorkspaceChildRunLeaseInTxRejectsParentResumeReceipt(t *testing.T) {
	worker, locators, authority := validSameWorkspaceChildRunLeaseClaimFixture(false)
	locators.EnclosingResumeWriterGeneration = pgtype.Int8{Int64: 6, Valid: true}
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimSameWorkspaceChildRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("parent resume receipt acquired child claim locks: %v", store.calls)
	}
}

func TestClaimSameWorkspaceChildRunLeaseInTxRejectsDifferentTarget(t *testing.T) {
	worker, locators, authority := validSameWorkspaceChildRunLeaseClaimFixture(false)
	authority.runWait.ChildTargetDeclaredID = pgtype.Text{String: "other-task", Valid: true}
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimSameWorkspaceChildRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
}

func TestClaimSameWorkspaceChildRunLeaseInTxExtendsEnclosingHandoff(t *testing.T) {
	worker, locators, authority := validSameWorkspaceChildRunLeaseClaimFixture(false)
	attachmentOwnerRunID := pgvalue.UUID(uuid.New())
	locators.ParentEnclosingWaitID = pgvalue.UUID(uuid.New())
	locators.ParentEnclosingRunID = attachmentOwnerRunID
	locators.ParentEnclosingAttemptNumber = 1
	authority.workspace.OwnerRunID = attachmentOwnerRunID
	authority.enclosingWait = activeEnclosingWaitFixture(
		locators.ParentEnclosingWaitID,
		attachmentOwnerRunID,
		authority.parentRun,
		authority,
		3,
		locators.EnclosingParentWriterGeneration.Int64,
	)
	store := &runLeaseClaimStore{authority: authority}

	if _, err := claimSameWorkspaceChildRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	); err != nil {
		t.Fatalf("%v after %v", err, store.calls)
	}
	if !slices.Equal(store.calls, []string{
		"parent_run", "run", "workspace", "parent_attempt", "attempt",
		"worker_group", "worker", "network_slot", "runtime", "run_lease",
		"workspace_mount", "workspace_lease", "mark_starting",
		"enclosing_wait", "handoff_wait", "checkpoint", "checkpoint_source",
	}) {
		t.Fatalf("lock order = %v", store.calls)
	}
}

func TestClaimSameWorkspaceChildRunLeaseInTxRejectsBrokenEnclosingChain(t *testing.T) {
	worker, locators, authority := validSameWorkspaceChildRunLeaseClaimFixture(false)
	attachmentOwnerRunID := pgvalue.UUID(uuid.New())
	locators.ParentEnclosingWaitID = pgvalue.UUID(uuid.New())
	locators.ParentEnclosingRunID = attachmentOwnerRunID
	locators.ParentEnclosingAttemptNumber = 1
	authority.workspace.OwnerRunID = attachmentOwnerRunID
	authority.enclosingWait = activeEnclosingWaitFixture(
		locators.ParentEnclosingWaitID,
		attachmentOwnerRunID,
		authority.parentRun,
		authority,
		3,
		locators.EnclosingParentWriterGeneration.Int64-1,
	)
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimSameWorkspaceChildRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
	if slices.Contains(store.calls, "handoff_wait") {
		t.Fatalf("broken outer edge reached inner Wait: %v", store.calls)
	}
}

func TestClaimSameWorkspaceParentResumeRunLeaseInTxUsesHandoffCheckpoint(t *testing.T) {
	for _, actorParent := range []bool{false, true} {
		worker, locators, authority := validSameWorkspaceParentResumeRunLeaseClaimFixture(actorParent, true)
		store := &runLeaseClaimStore{authority: authority}

		claimed, err := claimSameWorkspaceParentResumeRunLeaseInTx(
			context.Background(),
			store,
			worker,
			authority.runLease.ID,
			authority.runLease.LeaseSequence,
			locators,
		)
		if err != nil {
			t.Fatalf("actorParent=%t: %v", actorParent, err)
		}
		wantOrder := []string{
			"run",
			"child_run",
			"workspace",
			"attempt",
			"child_attempt",
			"worker_group",
			"worker",
			"network_slot",
			"runtime",
			"run_lease",
			"workspace_mount",
			"workspace_lease",
			"mark_starting",
			"run_wait",
			"checkpoint",
			"checkpoint_source",
		}
		if actorParent {
			wantOrder = append([]string{"actor"}, wantOrder...)
		}
		if !slices.Equal(store.calls, wantOrder) {
			t.Fatalf("actorParent=%t lock order = %v, want %v", actorParent, store.calls, wantOrder)
		}
		if claimed.checkpoint.Kind != db.RunCheckpointKindHandoffResume {
			t.Fatalf("checkpoint kind = %s", claimed.checkpoint.Kind)
		}
	}
}

func TestClaimSameWorkspaceParentResumeRunLeaseInTxUpdatesEnclosingAuthority(t *testing.T) {
	worker, locators, authority := validSameWorkspaceParentResumeRunLeaseClaimFixture(false, true)
	attachmentOwnerRunID := pgvalue.UUID(uuid.New())
	authority.parentRun = db.Run{
		ID:                   attachmentOwnerRunID,
		DeploymentID:         authority.run.DeploymentID,
		WorkspaceID:          authority.run.WorkspaceID,
		Status:               db.RunStatusWaiting,
		CurrentAttemptNumber: 1,
	}
	authority.run.ParentRunID = attachmentOwnerRunID
	authority.run.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	authority.run.EntrypointDeclaredID = "nested-task"
	authority.workspace.OwnerRunID = attachmentOwnerRunID
	locators.ParentRunID = attachmentOwnerRunID
	locators.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	locators.ParentAttemptNumber = 1
	authority.enclosingWait = activeEnclosingWaitFixture(
		pgvalue.UUID(uuid.New()),
		attachmentOwnerRunID,
		authority.run,
		authority,
		3,
		authority.workspace.WriterGeneration,
	)
	setEnclosingLocator(&locators, authority.enclosingWait)
	store := &runLeaseClaimStore{authority: authority}

	if _, err := claimSameWorkspaceParentResumeRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.calls, []string{
		"parent_run", "run", "child_run", "workspace", "attempt", "child_attempt",
		"worker_group", "worker", "network_slot", "runtime", "run_lease",
		"workspace_mount", "workspace_lease", "mark_starting",
		"enclosing_wait", "run_wait", "checkpoint", "checkpoint_source",
	}) {
		t.Fatalf("lock order = %v", store.calls)
	}
}

func TestClaimSameWorkspaceParentResumeRunLeaseInTxUsesSuspendCheckpointAfterChildFailure(t *testing.T) {
	worker, locators, authority := validSameWorkspaceParentResumeRunLeaseClaimFixture(false, false)
	store := &runLeaseClaimStore{authority: authority}

	claimed, err := claimSameWorkspaceParentResumeRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.checkpoint.Kind != db.RunCheckpointKindSuspend {
		t.Fatalf("checkpoint kind = %s", claimed.checkpoint.Kind)
	}
	if claimed.workspaceLease.BaseVersionID != claimed.runWait.BaseWorkspaceVersionID {
		t.Fatal("failed child did not restore pre-child base")
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxPreservesEnclosingHandoff(t *testing.T) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(false)
	attachmentOwnerRunID := pgvalue.UUID(uuid.New())
	authority.parentRun = db.Run{
		ID:                   attachmentOwnerRunID,
		DeploymentID:         authority.run.DeploymentID,
		WorkspaceID:          authority.run.WorkspaceID,
		Status:               db.RunStatusWaiting,
		CurrentAttemptNumber: 1,
	}
	authority.run.ParentRunID = attachmentOwnerRunID
	authority.run.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	authority.run.EntrypointDeclaredID = "nested-task"
	authority.workspace.OwnerRunID = attachmentOwnerRunID
	authority.sourceRuntime = authority.runtime
	authority.sourceRunLease.RuntimeInstanceID = authority.runtime.ID
	authority.sourceWorkspaceLease.WorkspaceMountID = authority.workspaceMount.ID
	authority.sourceWorkspaceLease.MountFencingGeneration = authority.workspaceMount.FencingGeneration
	locators.ParentRunID = attachmentOwnerRunID
	locators.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	locators.ParentAttemptNumber = 1
	authority.enclosingWait = activeEnclosingWaitFixture(
		pgvalue.UUID(uuid.New()),
		attachmentOwnerRunID,
		authority.run,
		authority,
		3,
		authority.workspace.WriterGeneration,
	)
	setEnclosingLocator(&locators, authority.enclosingWait)
	store := &runLeaseClaimStore{authority: authority}

	if _, err := claimCheckpointRestoreRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	); err != nil {
		t.Fatalf("%v after %v", err, store.calls)
	}
	if !slices.Equal(store.calls, []string{
		"parent_run", "run", "workspace", "attempt",
		"worker_group", "worker", "network_slot", "runtime", "run_lease",
		"workspace_mount", "workspace_lease", "mark_starting",
		"enclosing_wait", "run_wait", "checkpoint", "checkpoint_source",
	}) {
		t.Fatalf("lock order = %v", store.calls)
	}
}

func TestClaimCheckpointRestoreRunLeaseInTxRejectsDifferentSourceWorkspaceLease(t *testing.T) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(false)
	authority.sourceWorkspaceLease.WriterGeneration = authority.workspace.WriterGeneration
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimCheckpointRestoreRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
}

func TestClaimSameWorkspaceParentResumeRunLeaseInTxRejectsDifferentMount(t *testing.T) {
	worker, locators, authority := validSameWorkspaceParentResumeRunLeaseClaimFixture(false, true)
	authority.runWait.HandoffWorkspaceMountID = pgvalue.UUID(uuid.New())
	store := &runLeaseClaimStore{authority: authority}

	_, err := claimSameWorkspaceParentResumeRunLeaseInTx(
		context.Background(),
		store,
		worker,
		authority.runLease.ID,
		authority.runLease.LeaseSequence,
		locators,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale claim", err)
	}
}

type runLeaseClaimStore struct {
	db.Querier
	authority      runLeaseClaimAuthority
	locators       db.GetRunLeaseClaimLocatorsRow
	secretLocators *db.GetRunLeaseSecretDeliveryLocatorsRow
	secretRows     []db.LockAttemptSecretDeliveryRow
	secretVersion  db.SecretVersion
	program        db.GetDeploymentProgramAuthorityRow
	definition     db.DeploymentDefinition
	projectionErr  error
	calls          []string
}

func (s *runLeaseClaimStore) BeginQuerier(context.Context) (db.Querier, controlTransaction, error) {
	return s, runLeaseClaimTransaction{store: s}, nil
}

func (s *runLeaseClaimStore) GetRunLeaseSecretDeliveryLocators(
	context.Context,
	db.GetRunLeaseSecretDeliveryLocatorsParams,
) (db.GetRunLeaseSecretDeliveryLocatorsRow, error) {
	s.calls = append(s.calls, "secret_locators")
	if s.secretLocators != nil {
		return *s.secretLocators, nil
	}
	return db.GetRunLeaseSecretDeliveryLocatorsRow{
		EnvironmentID: s.locators.EnvironmentID,
		RunID:         s.locators.RunID,
		WorkspaceID:   s.locators.WorkspaceID,
		AttemptNumber: s.locators.AttemptNumber,
	}, nil
}

func (s *runLeaseClaimStore) LockAttemptSecretDelivery(
	context.Context,
	db.LockAttemptSecretDeliveryParams,
) ([]db.LockAttemptSecretDeliveryRow, error) {
	s.calls = append(s.calls, "secrets")
	return s.secretRows, nil
}

func (s *runLeaseClaimStore) GetSecretVersion(
	context.Context,
	db.GetSecretVersionParams,
) (db.SecretVersion, error) {
	s.calls = append(s.calls, "secret_version")
	return s.secretVersion, nil
}

func (s *runLeaseClaimStore) GetRunLeaseClaimLocators(
	context.Context,
	db.GetRunLeaseClaimLocatorsParams,
) (db.GetRunLeaseClaimLocatorsRow, error) {
	s.calls = append(s.calls, "locators")
	return s.locators, nil
}

func (s *runLeaseClaimStore) GetDeploymentProgramAuthority(
	context.Context,
	db.GetDeploymentProgramAuthorityParams,
) (db.GetDeploymentProgramAuthorityRow, error) {
	s.calls = append(s.calls, "program")
	if s.projectionErr != nil {
		return db.GetDeploymentProgramAuthorityRow{}, s.projectionErr
	}
	return s.program, nil
}

func (s *runLeaseClaimStore) GetDeploymentDefinition(
	context.Context,
	db.GetDeploymentDefinitionParams,
) (db.DeploymentDefinition, error) {
	s.calls = append(s.calls, "definition")
	if s.projectionErr != nil {
		return db.DeploymentDefinition{}, s.projectionErr
	}
	return s.definition, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimActor(context.Context, db.LockRunLeaseClaimActorParams) (db.Actor, error) {
	s.calls = append(s.calls, "actor")
	return s.authority.actor, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimRun(_ context.Context, params db.LockRunLeaseClaimRunParams) (db.Run, error) {
	if s.authority.parentRun.ID.Valid && params.ID == s.authority.parentRun.ID {
		s.calls = append(s.calls, "parent_run")
		return s.authority.parentRun, nil
	}
	if s.authority.childRun.ID.Valid && params.ID == s.authority.childRun.ID {
		s.calls = append(s.calls, "child_run")
		return s.authority.childRun, nil
	}
	s.calls = append(s.calls, "run")
	return s.authority.run, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimWorkspace(context.Context, db.LockRunLeaseClaimWorkspaceParams) (db.Workspace, error) {
	s.calls = append(s.calls, "workspace")
	return s.authority.workspace, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimAttempt(_ context.Context, params db.LockRunLeaseClaimAttemptParams) (db.RunAttempt, error) {
	if s.authority.parentAttempt.RunID.Valid && params.RunID == s.authority.parentAttempt.RunID {
		s.calls = append(s.calls, "parent_attempt")
		return s.authority.parentAttempt, nil
	}
	if s.authority.childAttempt.RunID.Valid && params.RunID == s.authority.childAttempt.RunID {
		s.calls = append(s.calls, "child_attempt")
		return s.authority.childAttempt, nil
	}
	s.calls = append(s.calls, "attempt")
	return s.authority.attempt, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimWorkerGroup(context.Context, db.LockRunLeaseClaimWorkerGroupParams) (db.WorkerGroup, error) {
	s.calls = append(s.calls, "worker_group")
	return s.authority.workerGroup, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimWorker(context.Context, db.LockRunLeaseClaimWorkerParams) (db.WorkerInstance, error) {
	s.calls = append(s.calls, "worker")
	return s.authority.worker, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimNetworkSlot(context.Context, db.LockRunLeaseClaimNetworkSlotParams) (db.WorkerNetworkSlot, error) {
	s.calls = append(s.calls, "network_slot")
	return s.authority.networkSlot, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimRuntime(context.Context, db.LockRunLeaseClaimRuntimeParams) (db.RuntimeInstance, error) {
	s.calls = append(s.calls, "runtime")
	return s.authority.runtime, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimLease(context.Context, db.LockRunLeaseClaimLeaseParams) (db.RunLease, error) {
	s.calls = append(s.calls, "run_lease")
	return s.authority.runLease, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimMount(context.Context, db.LockRunLeaseClaimMountParams) (db.WorkspaceMount, error) {
	s.calls = append(s.calls, "workspace_mount")
	return s.authority.workspaceMount, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimWorkspaceLease(context.Context, db.LockRunLeaseClaimWorkspaceLeaseParams) (db.WorkspaceLease, error) {
	s.calls = append(s.calls, "workspace_lease")
	return s.authority.workspaceLease, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimWait(context.Context, db.LockRunLeaseClaimWaitParams) (db.RunWait, error) {
	s.calls = append(s.calls, "run_wait")
	return s.authority.runWait, nil
}

func (s *runLeaseClaimStore) LockSameWorkspaceHandoffWait(_ context.Context, params db.LockSameWorkspaceHandoffWaitParams) (db.RunWait, error) {
	if s.authority.enclosingWait.ID.Valid && params.ID == s.authority.enclosingWait.ID {
		s.calls = append(s.calls, "enclosing_wait")
		return s.authority.enclosingWait, nil
	}
	s.calls = append(s.calls, "handoff_wait")
	return s.authority.runWait, nil
}

func (s *runLeaseClaimStore) LockRestorableRunCheckpoint(context.Context, db.LockRestorableRunCheckpointParams) (db.RunCheckpoint, error) {
	s.calls = append(s.calls, "checkpoint")
	return s.authority.checkpoint, nil
}

func (s *runLeaseClaimStore) LockReadyRunCheckpoint(context.Context, db.LockReadyRunCheckpointParams) (db.RunCheckpoint, error) {
	s.calls = append(s.calls, "checkpoint")
	return s.authority.checkpoint, nil
}

func (s *runLeaseClaimStore) GetRunCheckpointSource(context.Context, db.GetRunCheckpointSourceParams) (db.GetRunCheckpointSourceRow, error) {
	s.calls = append(s.calls, "checkpoint_source")
	return db.GetRunCheckpointSourceRow{
		RunLease:        s.authority.sourceRunLease,
		WorkspaceLease:  s.authority.sourceWorkspaceLease,
		RuntimeInstance: s.authority.sourceRuntime,
	}, nil
}

func (s *runLeaseClaimStore) MarkRunLeaseStarting(context.Context, db.MarkRunLeaseStartingParams) (db.RunLease, error) {
	s.calls = append(s.calls, "mark_starting")
	lease := s.authority.runLease
	lease.State = db.RunLeaseStateStarting
	lease.ClaimedAt = pgtype.Timestamptz{Valid: true}
	s.authority.runLease = lease
	return lease, nil
}

type runLeaseClaimTransaction struct {
	store *runLeaseClaimStore
}

func (tx runLeaseClaimTransaction) Commit(context.Context) error {
	tx.store.calls = append(tx.store.calls, "commit")
	return nil
}

func (tx runLeaseClaimTransaction) Rollback(context.Context) error {
	tx.store.calls = append(tx.store.calls, "rollback")
	return nil
}

func validRunLeaseClaimSecretFixture(
	locators db.GetRunLeaseClaimLocatorsRow,
) (db.LockAttemptSecretDeliveryRow, db.SecretVersion) {
	secretID := pgvalue.UUID(uuid.New())
	versionID := pgvalue.UUID(uuid.New())
	secretRow := db.Secret{
		ID:                   secretID,
		EnvironmentID:        locators.EnvironmentID,
		State:                "active",
		CurrentVersionID:     versionID,
		RevocationGeneration: 2,
	}
	return db.LockAttemptSecretDeliveryRow{
			WorkspaceSecret: db.WorkspaceSecret{
				WorkspaceID:     locators.WorkspaceID,
				EnvironmentID:   locators.EnvironmentID,
				PlacementKind:   "env",
				PlacementTarget: "API_KEY",
				SecretID:        secretID,
			},
			Secret:                         secretRow,
			ResolutionID:                   pgvalue.UUID(uuid.New()),
			ResolutionRunID:                locators.RunID,
			ResolutionAttemptNumber:        pgtype.Int4{Int32: locators.AttemptNumber, Valid: true},
			ResolutionSecretVersionID:      versionID,
			ResolutionRevocationGeneration: pgtype.Int8{Int64: 2, Valid: true},
		}, db.SecretVersion{
			ID:       versionID,
			SecretID: secretID,
			Version:  1,
		}
}

func validRunLeaseClaimFixture() (workerActor, db.GetRunLeaseClaimLocatorsRow, runLeaseClaimAuthority) {
	id := func() pgtype.UUID {
		return pgvalue.UUID(uuid.New())
	}
	orgID := id()
	projectID := id()
	environmentID := id()
	runID := id()
	workspaceID := id()
	deploymentID := id()
	definitionID := id()
	workerInstanceID := uuid.New()
	runtimeID := id()
	networkSlotID := id()
	runLeaseID := id()
	mountID := id()
	workspaceLeaseID := id()
	versionID := id()
	const (
		workerGroupID  = "run-workers"
		regionID       = "us-east-1"
		protocol       = "helmr.worker.v0"
		runtimeIDValue = "runtime-identity"
	)
	worker := workerActor{
		WorkerInstanceID:  workerInstanceID,
		WorkerGroupID:     workerGroupID,
		WorkerEpoch:       7,
		ClaimVersion:      3,
		GroupClaimVersion: 1,
		ProtocolVersion:   protocol,
	}
	locators := db.GetRunLeaseClaimLocatorsRow{
		OrgID:                 orgID,
		ProjectID:             projectID,
		EnvironmentID:         environmentID,
		RunID:                 runID,
		WorkspaceID:           workspaceID,
		AttemptNumber:         1,
		RegionID:              regionID,
		RuntimeInstanceID:     runtimeID,
		NetworkSlotID:         networkSlotID,
		NetworkSlotGeneration: 2,
		WorkspaceLeaseID:      workspaceLeaseID,
		WorkspaceMountID:      mountID,
	}
	authority := runLeaseClaimAuthority{
		run: db.Run{
			ID:                     runID,
			DeploymentID:           deploymentID,
			DeploymentDefinitionID: definitionID,
			EntrypointKind:         "task",
			WorkspaceID:            workspaceID,
			BaseWorkspaceVersionID: versionID,
			Status:                 db.RunStatusQueued,
			CurrentAttemptNumber:   1,
			CurrentRunLeaseID:      runLeaseID,
		},
		workspace: db.Workspace{
			ID:                     workspaceID,
			DeploymentDefinitionID: definitionID,
			OwnerRunID:             runID,
			OwnershipGeneration:    4,
			WriterGeneration:       5,
			HeadVersionID:          versionID,
			State:                  db.WorkspaceStateActive,
			DesiredState:           db.WorkspaceDesiredStateActive,
		},
		attempt: db.RunAttempt{
			RunID:                  runID,
			Number:                 1,
			EntrypointKind:         "task",
			WorkspaceID:            workspaceID,
			BaseWorkspaceVersionID: versionID,
		},
		workerGroup: db.WorkerGroup{
			ID:              workerGroupID,
			RegionID:        regionID,
			State:           db.WorkerGroupStateActive,
			ClaimVersion:    1,
			AllowsRun:       true,
			ProtocolVersion: protocol,
		},
		worker: db.WorkerInstance{
			ID:                     pgvalue.UUID(workerInstanceID),
			WorkerGroupID:          workerGroupID,
			State:                  db.WorkerInstanceStateActive,
			ClaimVersion:           3,
			CurrentEpoch:           pgtype.Int8{Int64: 7, Valid: true},
			ProtocolVersion:        protocol,
			SupportsRun:            true,
			RuntimeIdentityID:      pgtype.Text{String: runtimeIDValue, Valid: true},
			PerVmCpuMillis:         1000,
			PerVmMemoryBytes:       2048,
			PerVmWorkloadDiskBytes: 4096,
			PerVmScratchBytes:      8192,
		},
		networkSlot: db.WorkerNetworkSlot{
			ID:                networkSlotID,
			State:             db.WorkerNetworkSlotStateBound,
			Generation:        2,
			RuntimeInstanceID: runtimeID,
		},
		runtime: db.RuntimeInstance{
			ID:                        runtimeID,
			RuntimeIdentityID:         runtimeIDValue,
			DeploymentDefinitionID:    definitionID,
			ReservedCpuMillis:         1000,
			ReservedMemoryBytes:       2048,
			ReservedWorkloadDiskBytes: 4096,
			ReservedScratchBytes:      8192,
			ReservedExecutionSlots:    1,
			ProgramDeploymentID:       deploymentID,
			DesiredState:              db.RuntimeDesiredStateReady,
			DesiredVersion:            1,
			ObservedState:             db.RuntimeObservedStateReady,
			ObservedDesiredVersion:    1,
		},
		runLease: db.RunLease{
			ID:                         runLeaseID,
			RunID:                      runID,
			WorkspaceID:                workspaceID,
			LeaseSequence:              1,
			AttemptNumber:              1,
			WorkerGroupID:              workerGroupID,
			WorkerInstanceID:           pgvalue.UUID(workerInstanceID),
			WorkerEpoch:                7,
			RuntimeInstanceID:          runtimeID,
			NetworkSlotID:              networkSlotID,
			NetworkSlotGeneration:      2,
			RuntimeIdentityID:          runtimeIDValue,
			WorkerProtocolVersion:      protocol,
			RequestedCpuMillis:         1000,
			RequestedMemoryBytes:       2048,
			RequestedWorkloadDiskBytes: 4096,
			RequestedScratchBytes:      8192,
			RequestedExecutionSlots:    1,
			State:                      db.RunLeaseStateAssigned,
		},
		workspaceMount: db.WorkspaceMount{
			ID:                    mountID,
			MaterializedVersionID: versionID,
			State:                 db.WorkspaceMountStateMounted,
			FencingGeneration:     6,
		},
		workspaceLease: db.WorkspaceLease{
			ID:                     workspaceLeaseID,
			State:                  db.WorkspaceLeaseStateActive,
			OwnerRunLeaseID:        runLeaseID,
			BaseVersionID:          versionID,
			OwnershipGeneration:    4,
			WriterGeneration:       5,
			MountFencingGeneration: 6,
		},
	}
	return worker, locators, authority
}

func validActorRunLeaseClaimFixture() (workerActor, db.GetRunLeaseClaimLocatorsRow, runLeaseClaimAuthority) {
	worker, locators, authority := validRunLeaseClaimFixture()
	actorID := pgvalue.UUID(uuid.New())
	locators.ActorID = actorID
	locators.ActorRunGeneration = pgtype.Int8{Int64: 9, Valid: true}
	authority.actor = db.Actor{
		ID:                     actorID,
		ActorDeclaredID:        "test-actor",
		DeploymentDefinitionID: authority.run.DeploymentDefinitionID,
		WorkspaceID:            authority.workspace.ID,
		CurrentRunID:           authority.run.ID,
		RunGeneration:          9,
		NextInputSequence:      4,
		CommittedInputSequence: 1,
		State:                  "open",
	}
	authority.run.EntrypointKind = "actor"
	authority.run.EntrypointDeclaredID = "test-actor"
	authority.run.ActorID = actorID
	authority.run.ActorStartInputSequence = pgtype.Int8{Int64: 1, Valid: true}
	authority.run.ActorStartInputHighWatermark = pgtype.Int8{Int64: 3, Valid: true}
	authority.workspace.OwnerRunID = pgtype.UUID{}
	authority.workspace.OwnerActorID = actorID
	authority.attempt.EntrypointKind = "actor"
	authority.attempt.ActorStartInputSequence = pgtype.Int8{Int64: 1, Valid: true}
	return worker, locators, authority
}

func validCheckpointRestoreRunLeaseClaimFixture(actor bool) (workerActor, db.GetRunLeaseClaimLocatorsRow, runLeaseClaimAuthority) {
	var worker workerActor
	var locators db.GetRunLeaseClaimLocatorsRow
	var authority runLeaseClaimAuthority
	if actor {
		worker, locators, authority = validActorRunLeaseClaimFixture()
	} else {
		worker, locators, authority = validRunLeaseClaimFixture()
	}

	runWaitID := pgvalue.UUID(uuid.New())
	sourceRunLeaseID := pgvalue.UUID(uuid.New())
	sourceWorkspaceLeaseID := pgvalue.UUID(uuid.New())
	checkpointID := pgvalue.UUID(uuid.New())
	resumeAttachID := pgvalue.UUID(uuid.New())
	privateVersionID := pgvalue.UUID(uuid.New())
	locators.RunWaitID = runWaitID
	locators.SuspendCheckpointID = checkpointID
	locators.ResumeAttachID = resumeAttachID
	locators.ResumeRequestVersion = pgtype.Int8{Int64: 2, Valid: true}
	locators.CheckpointPrivateWorkspaceVersionID = privateVersionID
	authority.workspaceMount.MaterializedVersionID = privateVersionID
	authority.workspaceLease.BaseVersionID = privateVersionID
	authority.attempt.EntrypointEnteredAt = pgtype.Timestamptz{Valid: true}
	authority.runWait = db.RunWait{
		ID:                       runWaitID,
		EnvironmentID:            locators.EnvironmentID,
		RunID:                    locators.RunID,
		WorkspaceID:              locators.WorkspaceID,
		ConditionState:           db.WaitStateCompleted,
		SuspensionState:          db.RunWaitStateResuming,
		AttemptNumber:            locators.AttemptNumber,
		CurrentRunLeaseID:        authority.runLease.ID,
		PriorRunLeaseID:          sourceRunLeaseID,
		CheckpointRequestVersion: 1,
		CheckpointAckVersion:     1,
		SuspendCheckpointID:      checkpointID,
		ResumeAttachID:           resumeAttachID,
		ResumeRequestVersion:     2,
		ResumeAckVersion:         1,
	}
	authority.checkpoint = db.RunCheckpoint{
		ID:                        checkpointID,
		Kind:                      db.RunCheckpointKindSuspend,
		RunID:                     locators.RunID,
		AttemptNumber:             locators.AttemptNumber,
		RunWaitID:                 runWaitID,
		SourceRunLeaseID:          sourceRunLeaseID,
		SourceWorkspaceLeaseID:    sourceWorkspaceLeaseID,
		WorkspaceID:               locators.WorkspaceID,
		BaseWorkspaceVersionID:    authority.attempt.BaseWorkspaceVersionID,
		PrivateWorkspaceVersionID: privateVersionID,
		State:                     db.RunCheckpointStateReady,
	}
	if actor {
		authority.checkpoint.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	}
	authority.sourceRuntime = authority.runtime
	authority.sourceRuntime.ID = pgvalue.UUID(uuid.New())
	authority.sourceRunLease = authority.runLease
	authority.sourceRunLease.ID = sourceRunLeaseID
	authority.sourceRunLease.RuntimeInstanceID = authority.sourceRuntime.ID
	authority.sourceRunLease.State = db.RunLeaseStateCheckpointed
	authority.sourceWorkspaceLease = db.WorkspaceLease{
		ID:                     sourceWorkspaceLeaseID,
		WorkspaceID:            locators.WorkspaceID,
		WorkspaceMountID:       pgvalue.UUID(uuid.New()),
		State:                  db.WorkspaceLeaseStateFenced,
		OwnerRunLeaseID:        sourceRunLeaseID,
		BaseVersionID:          authority.checkpoint.BaseWorkspaceVersionID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration,
		WriterGeneration:       authority.workspace.WriterGeneration - 1,
		MountFencingGeneration: 1,
	}
	return worker, locators, authority
}

func validSameWorkspaceChildRunLeaseClaimFixture(actorParent bool) (workerActor, db.GetRunLeaseClaimLocatorsRow, runLeaseClaimAuthority) {
	worker, locators, authority := validRunLeaseClaimFixture()
	parentRunID := pgvalue.UUID(uuid.New())
	checkpointID := pgvalue.UUID(uuid.New())
	sourceRunLeaseID := pgvalue.UUID(uuid.New())
	resumeAttachID := pgvalue.UUID(uuid.New())
	runWaitID := pgvalue.UUID(uuid.New())

	locators.ParentRunID = parentRunID
	locators.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	locators.ParentAttemptNumber = 1
	locators.EnclosingWaitID = runWaitID
	locators.EnclosingSuspendCheckpointID = checkpointID
	locators.EnclosingResumeAttachID = resumeAttachID
	locators.EnclosingBaseWorkspaceVersionID = authority.attempt.BaseWorkspaceVersionID
	locators.EnclosingRuntimeInstanceID = authority.runtime.ID
	locators.EnclosingWorkspaceMountID = authority.workspaceMount.ID
	locators.EnclosingMountGeneration = pgtype.Int8{Int64: authority.workspaceMount.FencingGeneration, Valid: true}
	locators.EnclosingOwnershipGeneration = pgtype.Int8{Int64: authority.workspace.OwnershipGeneration, Valid: true}
	locators.EnclosingParentWriterGeneration = pgtype.Int8{Int64: 4, Valid: true}
	locators.EnclosingChildWriterGeneration = pgtype.Int8{Int64: 5, Valid: true}

	authority.run.ParentRunID = parentRunID
	authority.run.ParentOwnsLifecycle = pgtype.Bool{Bool: true, Valid: true}
	authority.parentRun = db.Run{
		ID:                     parentRunID,
		DeploymentID:           authority.run.DeploymentID,
		DeploymentDefinitionID: authority.run.DeploymentDefinitionID,
		EntrypointKind:         "task",
		EntrypointDeclaredID:   "parent-task",
		WorkspaceID:            authority.run.WorkspaceID,
		BaseWorkspaceVersionID: authority.attempt.BaseWorkspaceVersionID,
		Status:                 db.RunStatusWaiting,
		CurrentAttemptNumber:   1,
	}
	authority.workspace.OwnerRunID = parentRunID
	authority.parentAttempt = db.RunAttempt{
		RunID:                  parentRunID,
		Number:                 1,
		EntrypointKind:         "task",
		WorkspaceID:            authority.workspace.ID,
		EntrypointEnteredAt:    pgtype.Timestamptz{Valid: true},
		BaseWorkspaceVersionID: authority.attempt.BaseWorkspaceVersionID,
	}
	authority.runWait = db.RunWait{
		ID:                         runWaitID,
		EnvironmentID:              locators.EnvironmentID,
		RunID:                      parentRunID,
		WorkspaceID:                locators.WorkspaceID,
		Kind:                       db.WaitKindChild,
		ConditionState:             db.WaitStatePending,
		SuspensionState:            db.RunWaitStateParked,
		AttemptNumber:              1,
		PriorRunLeaseID:            sourceRunLeaseID,
		CheckpointRequestVersion:   1,
		CheckpointAckVersion:       1,
		SuspendCheckpointID:        checkpointID,
		ResumeAttachID:             resumeAttachID,
		ChildRunID:                 authority.run.ID,
		ChildParentOwned:           pgtype.Bool{Bool: true, Valid: true},
		ChildTargetDeclaredID:      pgtype.Text{String: authority.run.EntrypointDeclaredID, Valid: true},
		BaseWorkspaceVersionID:     authority.attempt.BaseWorkspaceVersionID,
		BaseWorkspaceContentDigest: pgtype.Text{String: "sha256:test", Valid: true},
		HandoffRuntimeInstanceID:   authority.runtime.ID,
		HandoffWorkspaceMountID:    authority.workspaceMount.ID,
		HandoffMountGeneration:     pgtype.Int8{Int64: authority.workspaceMount.FencingGeneration, Valid: true},
		OwnershipGeneration:        pgtype.Int8{Int64: authority.workspace.OwnershipGeneration, Valid: true},
		ParentWriterGeneration:     pgtype.Int8{Int64: 4, Valid: true},
		ChildWriterGeneration:      pgtype.Int8{Int64: 5, Valid: true},
	}
	authority.checkpoint = db.RunCheckpoint{
		ID:                        checkpointID,
		Kind:                      db.RunCheckpointKindSuspend,
		RunID:                     parentRunID,
		AttemptNumber:             1,
		RunWaitID:                 runWaitID,
		SourceRunLeaseID:          sourceRunLeaseID,
		SourceWorkspaceLeaseID:    pgvalue.UUID(uuid.New()),
		WorkspaceID:               locators.WorkspaceID,
		BaseWorkspaceVersionID:    authority.parentAttempt.BaseWorkspaceVersionID,
		PrivateWorkspaceVersionID: authority.runWait.BaseWorkspaceVersionID,
		State:                     db.RunCheckpointStateReady,
	}
	authority.sourceRuntime = authority.runtime
	authority.sourceRunLease = authority.runLease
	authority.sourceRunLease.ID = sourceRunLeaseID
	authority.sourceRunLease.State = db.RunLeaseStateCheckpointed
	authority.sourceWorkspaceLease = db.WorkspaceLease{
		ID:                     authority.checkpoint.SourceWorkspaceLeaseID,
		WorkspaceID:            locators.WorkspaceID,
		WorkspaceMountID:       authority.workspaceMount.ID,
		State:                  db.WorkspaceLeaseStateFenced,
		OwnerRunLeaseID:        sourceRunLeaseID,
		BaseVersionID:          authority.checkpoint.BaseWorkspaceVersionID,
		OwnershipGeneration:    authority.workspace.OwnershipGeneration,
		WriterGeneration:       4,
		MountFencingGeneration: authority.workspaceMount.FencingGeneration,
	}
	if actorParent {
		actorID := pgvalue.UUID(uuid.New())
		locators.ParentActorID = actorID
		locators.ParentActorRunGeneration = pgtype.Int8{Int64: 7, Valid: true}
		authority.actor = db.Actor{
			ID:                     actorID,
			ActorDeclaredID:        "parent-actor",
			DeploymentDefinitionID: authority.parentRun.DeploymentDefinitionID,
			WorkspaceID:            authority.workspace.ID,
			CurrentRunID:           parentRunID,
			RunGeneration:          7,
			NextInputSequence:      4,
			CommittedInputSequence: 2,
			State:                  "open",
		}
		authority.parentRun.EntrypointKind = "actor"
		authority.parentRun.EntrypointDeclaredID = "parent-actor"
		authority.parentRun.ActorID = actorID
		authority.parentRun.ActorStartInputSequence = pgtype.Int8{Int64: 1, Valid: true}
		authority.parentRun.ActorStartInputHighWatermark = pgtype.Int8{Int64: 3, Valid: true}
		authority.parentAttempt.EntrypointKind = "actor"
		authority.parentAttempt.ActorStartInputSequence = pgtype.Int8{Int64: 1, Valid: true}
		authority.workspace.OwnerRunID = pgtype.UUID{}
		authority.workspace.OwnerActorID = actorID
		authority.checkpoint.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	}
	return worker, locators, authority
}

func activeEnclosingWaitFixture(
	waitID pgtype.UUID,
	parentRunID pgtype.UUID,
	child db.Run,
	authority runLeaseClaimAuthority,
	parentWriterGeneration int64,
	childWriterGeneration int64,
) db.RunWait {
	return db.RunWait{
		ID:                         waitID,
		EnvironmentID:              authority.runWait.EnvironmentID,
		RunID:                      parentRunID,
		WorkspaceID:                authority.workspace.ID,
		Kind:                       db.WaitKindChild,
		ConditionState:             db.WaitStatePending,
		SuspensionState:            db.RunWaitStateParked,
		AttemptNumber:              1,
		PriorRunLeaseID:            pgvalue.UUID(uuid.New()),
		CheckpointRequestVersion:   1,
		CheckpointAckVersion:       1,
		SuspendCheckpointID:        pgvalue.UUID(uuid.New()),
		ResumeAttachID:             pgvalue.UUID(uuid.New()),
		ChildRunID:                 child.ID,
		ChildParentOwned:           pgtype.Bool{Bool: true, Valid: true},
		ChildTargetDeclaredID:      pgtype.Text{String: child.EntrypointDeclaredID, Valid: true},
		BaseWorkspaceVersionID:     child.BaseWorkspaceVersionID,
		BaseWorkspaceContentDigest: pgtype.Text{String: "sha256:outer", Valid: true},
		HandoffRuntimeInstanceID:   authority.runtime.ID,
		HandoffWorkspaceMountID:    authority.workspaceMount.ID,
		HandoffMountGeneration:     pgtype.Int8{Int64: authority.workspaceMount.FencingGeneration, Valid: true},
		OwnershipGeneration:        pgtype.Int8{Int64: authority.workspace.OwnershipGeneration, Valid: true},
		ParentWriterGeneration:     pgtype.Int8{Int64: parentWriterGeneration, Valid: true},
		ChildWriterGeneration:      pgtype.Int8{Int64: childWriterGeneration, Valid: true},
	}
}

func setEnclosingLocator(locators *db.GetRunLeaseClaimLocatorsRow, wait db.RunWait) {
	locators.EnclosingWaitID = wait.ID
	locators.EnclosingSuspendCheckpointID = wait.SuspendCheckpointID
	locators.EnclosingResumeAttachID = wait.ResumeAttachID
	locators.EnclosingBaseWorkspaceVersionID = wait.BaseWorkspaceVersionID
	locators.EnclosingRuntimeInstanceID = wait.HandoffRuntimeInstanceID
	locators.EnclosingWorkspaceMountID = wait.HandoffWorkspaceMountID
	locators.EnclosingMountGeneration = wait.HandoffMountGeneration
	locators.EnclosingOwnershipGeneration = wait.OwnershipGeneration
	locators.EnclosingParentWriterGeneration = wait.ParentWriterGeneration
	locators.EnclosingChildWriterGeneration = wait.ChildWriterGeneration
	locators.EnclosingResumeWriterGeneration = wait.ResumeWriterGeneration
}

func validSameWorkspaceParentResumeRunLeaseClaimFixture(
	actorParent bool,
	childSucceeded bool,
) (workerActor, db.GetRunLeaseClaimLocatorsRow, runLeaseClaimAuthority) {
	worker, locators, authority := validCheckpointRestoreRunLeaseClaimFixture(actorParent)
	childRunID := pgvalue.UUID(uuid.New())
	resultVersionID := pgvalue.UUID(uuid.New())

	locators.ResumeChildRunID = childRunID
	locators.ResumeChildAttemptNumber = 1
	locators.HandoffResumeWorkspaceVersionID = resultVersionID
	locators.ResumeHandoffRuntimeInstanceID = authority.runtime.ID
	locators.ResumeHandoffWorkspaceMountID = authority.workspaceMount.ID
	locators.ResumeHandoffMountGeneration = pgtype.Int8{Int64: authority.workspaceMount.FencingGeneration, Valid: true}
	locators.ResumeHandoffOwnershipGeneration = pgtype.Int8{Int64: authority.workspace.OwnershipGeneration, Valid: true}
	locators.ResumeHandoffParentWriterGeneration = pgtype.Int8{Int64: 4, Valid: true}
	locators.ResumeHandoffChildWriterGeneration = pgtype.Int8{Int64: 5, Valid: true}
	locators.ResumeHandoffResumeWriterGeneration = pgtype.Int8{Int64: 6, Valid: true}

	authority.childRun = db.Run{
		ID:                     childRunID,
		DeploymentID:           authority.run.DeploymentID,
		DeploymentDefinitionID: authority.run.DeploymentDefinitionID,
		EntrypointKind:         "task",
		EntrypointDeclaredID:   "child-task",
		ParentRunID:            authority.run.ID,
		ParentOwnsLifecycle:    pgtype.Bool{Bool: true, Valid: true},
		WorkspaceID:            authority.workspace.ID,
		BaseWorkspaceVersionID: authority.attempt.BaseWorkspaceVersionID,
		CurrentAttemptNumber:   1,
	}
	authority.childAttempt = db.RunAttempt{
		RunID:                  childRunID,
		Number:                 1,
		EntrypointKind:         "task",
		WorkspaceID:            authority.workspace.ID,
		EntrypointEnteredAt:    pgtype.Timestamptz{Valid: true},
		BaseWorkspaceVersionID: authority.attempt.BaseWorkspaceVersionID,
		TerminalOutcome:        pgtype.Text{String: "succeeded", Valid: true},
		TerminalAt:             pgtype.Timestamptz{Valid: true},
	}
	authority.workspace.WriterGeneration = 6
	authority.workspaceLease.WriterGeneration = 6
	authority.runWait.Kind = db.WaitKindChild
	authority.runWait.ChildRunID = childRunID
	authority.runWait.ChildParentOwned = pgtype.Bool{Bool: true, Valid: true}
	authority.runWait.ChildTargetDeclaredID = pgtype.Text{String: "child-task", Valid: true}
	authority.runWait.BaseWorkspaceVersionID = authority.attempt.BaseWorkspaceVersionID
	authority.runWait.BaseWorkspaceContentDigest = pgtype.Text{String: "sha256:test", Valid: true}
	authority.runWait.ResumeWorkspaceVersionID = resultVersionID
	authority.runWait.HandoffRuntimeInstanceID = authority.runtime.ID
	authority.runWait.HandoffWorkspaceMountID = authority.workspaceMount.ID
	authority.runWait.HandoffMountGeneration = pgtype.Int8{Int64: authority.workspaceMount.FencingGeneration, Valid: true}
	authority.runWait.OwnershipGeneration = pgtype.Int8{Int64: authority.workspace.OwnershipGeneration, Valid: true}
	authority.runWait.ParentWriterGeneration = pgtype.Int8{Int64: 4, Valid: true}
	authority.runWait.ChildWriterGeneration = pgtype.Int8{Int64: 5, Valid: true}
	authority.runWait.ResumeWriterGeneration = pgtype.Int8{Int64: 6, Valid: true}
	authority.workspaceMount.MaterializedVersionID = resultVersionID
	authority.workspaceLease.BaseVersionID = resultVersionID
	authority.checkpoint.Kind = db.RunCheckpointKindHandoffResume
	authority.checkpoint.PrivateWorkspaceVersionID = resultVersionID
	authority.sourceRuntime = authority.runtime
	authority.sourceRunLease.RuntimeInstanceID = authority.runtime.ID
	authority.sourceWorkspaceLease.WorkspaceMountID = authority.workspaceMount.ID
	authority.sourceWorkspaceLease.MountFencingGeneration = authority.workspaceMount.FencingGeneration

	if childSucceeded {
		authority.checkpoint.ID = pgvalue.UUID(uuid.New())
		authority.childRun.Status = db.RunStatusSucceeded
		authority.runWait.ConditionState = db.WaitStateCompleted
		authority.runWait.ChildResultVersionID = resultVersionID
		authority.runWait.HandoffResumeCheckpointID = authority.checkpoint.ID
		locators.HandoffResumeCheckpointID = authority.checkpoint.ID
	} else {
		authority.childRun.Status = db.RunStatusFailed
		authority.childAttempt.TerminalOutcome = pgtype.Text{String: "failed", Valid: true}
		authority.runWait.ConditionState = db.WaitStateFailed
		authority.runWait.ResumeWorkspaceVersionID = authority.runWait.BaseWorkspaceVersionID
		locators.HandoffResumeWorkspaceVersionID = authority.runWait.BaseWorkspaceVersionID
		authority.workspaceMount.MaterializedVersionID = authority.runWait.BaseWorkspaceVersionID
		authority.workspaceLease.BaseVersionID = authority.runWait.BaseWorkspaceVersionID
		authority.checkpoint.Kind = db.RunCheckpointKindSuspend
		authority.checkpoint.PrivateWorkspaceVersionID = authority.runWait.BaseWorkspaceVersionID
		locators.HandoffResumeCheckpointID = pgtype.UUID{}
	}
	return worker, locators, authority
}
