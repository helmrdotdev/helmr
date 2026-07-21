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

type runLeaseClaimStore struct {
	db.Querier
	authority runLeaseClaimAuthority
	calls     []string
}

func (s *runLeaseClaimStore) LockRunLeaseClaimRun(context.Context, db.LockRunLeaseClaimRunParams) (db.Run, error) {
	s.calls = append(s.calls, "run")
	return s.authority.run, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimWorkspace(context.Context, db.LockRunLeaseClaimWorkspaceParams) (db.Workspace, error) {
	s.calls = append(s.calls, "workspace")
	return s.authority.workspace, nil
}

func (s *runLeaseClaimStore) LockRunLeaseClaimAttempt(context.Context, db.LockRunLeaseClaimAttemptParams) (db.RunAttempt, error) {
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

func (s *runLeaseClaimStore) MarkRunLeaseStarting(context.Context, db.MarkRunLeaseStartingParams) (db.RunLease, error) {
	s.calls = append(s.calls, "mark_starting")
	lease := s.authority.runLease
	lease.State = db.RunLeaseStateStarting
	lease.ClaimedAt = pgtype.Timestamptz{Valid: true}
	s.authority.runLease = lease
	return lease, nil
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
