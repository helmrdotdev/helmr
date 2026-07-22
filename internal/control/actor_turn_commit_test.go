package control

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCommitActorTurnAdvancesOnlyTheNextInputCursor(t *testing.T) {
	server, store, worker, request, commit := newActorTurnCommitFixture(t)

	response, err := server.commitActorTurn(context.Background(), worker, request, commit)
	if err != nil {
		t.Fatal(err)
	}
	if store.cursorWrites != 1 || store.cursor.ExpectedInputSequence != 1 ||
		store.cursor.TargetInputSequence != 2 {
		t.Fatalf("cursor write = %+v, count = %d", store.cursor, store.cursorWrites)
	}
	if response.CommittedInputSequence != 2 || response.CorrelationID != request.CorrelationID ||
		response.WorkspaceVersionID != request.BaseWorkspaceVersionID {
		t.Fatalf("response = %+v", response)
	}
	if store.runLeaseClaimStore.authority.workspace.HeadVersionID != pgvalue.UUID(commit.baseVersionID) {
		t.Fatal("unchanged turn advanced the Workspace head")
	}

	replayed, err := server.commitActorTurn(context.Background(), worker, request, commit)
	if err != nil {
		t.Fatal(err)
	}
	if store.cursorWrites != 1 {
		t.Fatalf("replay cursor writes = %d, want 1", store.cursorWrites)
	}
	if replayed != response {
		t.Fatalf("replayed response = %+v, want %+v", replayed, response)
	}
}

func TestCommitActorTurnRejectsSkippedInputSequence(t *testing.T) {
	server, store, worker, request, commit := newActorTurnCommitFixture(t)
	commit.targetInputSequence = 3
	request.TargetInputSequence = 3

	_, err := server.commitActorTurn(context.Background(), worker, request, commit)
	if !errors.Is(err, errStaleActorTurnCommit) {
		t.Fatalf("error = %v, want stale Actor turn", err)
	}
	if store.cursorWrites != 0 {
		t.Fatal("stale turn advanced the Actor cursor")
	}
}

func TestCommitActorTurnRollsBackChangedWorkspaceWhenCursorAdvanceFails(t *testing.T) {
	server, store, worker, request, _ := newActorTurnCommitFixture(t)
	root := t.TempDir()
	if err := os.WriteFile(root+"/state.txt", []byte("updated"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := workspace.InspectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(root, t.TempDir(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	body, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	request.Tree = api.WorkerWorkspaceTreeIdentity{
		Digest: tree.Digest, SizeBytes: tree.SizeBytes, EntryCount: int32(tree.EntryCount),
	}
	request.Artifact = &api.WorkerWorkspaceArtifact{
		Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
		SizeBytes: artifact.SizeBytes, EntryCount: int32(artifact.EntryCount),
	}
	commit, err := parseActorTurnCommitRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	server.cas = actorTurnCAS{object: cas.Object{
		Digest: artifact.Digest, MediaType: artifact.MediaType, SizeBytes: artifact.SizeBytes,
	}, body: body}
	injected := errors.New("cursor write failed")
	store.cursorErr = injected
	originalHead := store.authority.workspace.HeadVersionID

	_, err = server.commitActorTurn(context.Background(), worker, request, commit)
	if !errors.Is(err, injected) {
		t.Fatalf("error = %v, want injected cursor failure", err)
	}
	if store.authority.workspace.HeadVersionID != originalHead ||
		store.authority.workspaceMount.MaterializedVersionID != originalHead ||
		store.authority.workspaceLease.BaseVersionID != originalHead {
		t.Fatal("rolled-back turn changed the durable Workspace frontier")
	}
	if store.cursorWrites != 0 || store.versionWrites != 0 || store.headWrites != 0 || store.leaseFrontierWrites != 0 {
		t.Fatalf("rolled-back writes persisted: cursor=%d version=%d head=%d lease=%d",
			store.cursorWrites, store.versionWrites, store.headWrites, store.leaseFrontierWrites)
	}
	if store.rollbacks != 1 || store.commits != 0 {
		t.Fatalf("transaction lifecycle: commits=%d rollbacks=%d", store.commits, store.rollbacks)
	}
}

type actorTurnCommitStore struct {
	*runLeaseClaimStore
	committedAt         pgtype.Timestamptz
	cursor              db.AdvanceActorTurnCursorParams
	cursorErr           error
	cursorWrites        int
	versionWrites       int
	headWrites          int
	leaseFrontierWrites int
	commits             int
	rollbacks           int
}

func (s *actorTurnCommitStore) BeginQuerier(context.Context) (db.Querier, controlTransaction, error) {
	base := *s.runLeaseClaimStore
	base.calls = append([]string(nil), s.runLeaseClaimStore.calls...)
	staged := &actorTurnCommitStore{
		runLeaseClaimStore: &base, committedAt: s.committedAt, cursor: s.cursor,
		cursorErr: s.cursorErr, cursorWrites: s.cursorWrites, versionWrites: s.versionWrites,
		headWrites: s.headWrites, leaseFrontierWrites: s.leaseFrontierWrites,
	}
	return staged, actorTurnCommitTransaction{parent: s, staged: staged}, nil
}

func (s *actorTurnCommitStore) GetTaskCompletionTime(context.Context) (pgtype.Timestamptz, error) {
	return s.committedAt, nil
}

func (s *actorTurnCommitStore) AdvanceActorTurnCursor(
	_ context.Context,
	params db.AdvanceActorTurnCursorParams,
) (db.Actor, error) {
	if s.cursorErr != nil {
		return db.Actor{}, s.cursorErr
	}
	s.cursor = params
	s.cursorWrites++
	s.authority.actor.CommittedInputSequence = params.TargetInputSequence
	return s.authority.actor, nil
}

func (s *actorTurnCommitStore) UpsertCasObject(context.Context, db.UpsertCasObjectParams) (db.CasObject, error) {
	return db.CasObject{}, nil
}

func (s *actorTurnCommitStore) CreateArtifact(_ context.Context, params db.CreateArtifactParams) (db.Artifact, error) {
	return db.Artifact{ID: params.ID}, nil
}

func (s *actorTurnCommitStore) PublishTaskWorkspaceVersion(
	_ context.Context,
	params db.PublishTaskWorkspaceVersionParams,
) (db.WorkspaceVersion, error) {
	s.versionWrites++
	return db.WorkspaceVersion{ID: params.ID}, nil
}

func (s *actorTurnCommitStore) UpdateTaskWorkspaceMountFrontier(
	_ context.Context,
	params db.UpdateTaskWorkspaceMountFrontierParams,
) (db.WorkspaceMount, error) {
	s.authority.workspaceMount.MaterializedVersionID = params.NewVersionID
	return s.authority.workspaceMount, nil
}

func (s *actorTurnCommitStore) AdvanceActorWorkspaceHead(
	_ context.Context,
	params db.AdvanceActorWorkspaceHeadParams,
) (db.Workspace, error) {
	s.headWrites++
	s.authority.workspace.HeadVersionID = params.NewHeadVersionID
	return s.authority.workspace, nil
}

func (s *actorTurnCommitStore) AdvanceActorTurnWorkspaceLeaseFrontier(
	_ context.Context,
	params db.AdvanceActorTurnWorkspaceLeaseFrontierParams,
) (db.WorkspaceLease, error) {
	s.leaseFrontierWrites++
	s.authority.workspaceLease.BaseVersionID = params.NewVersionID
	return s.authority.workspaceLease, nil
}

type actorTurnCommitTransaction struct {
	parent *actorTurnCommitStore
	staged *actorTurnCommitStore
}

func (tx actorTurnCommitTransaction) Commit(context.Context) error {
	parentCommits := tx.parent.commits + 1
	parentRollbacks := tx.parent.rollbacks
	*tx.parent.runLeaseClaimStore = *tx.staged.runLeaseClaimStore
	tx.parent.committedAt = tx.staged.committedAt
	tx.parent.cursor = tx.staged.cursor
	tx.parent.cursorErr = tx.staged.cursorErr
	tx.parent.cursorWrites = tx.staged.cursorWrites
	tx.parent.versionWrites = tx.staged.versionWrites
	tx.parent.headWrites = tx.staged.headWrites
	tx.parent.leaseFrontierWrites = tx.staged.leaseFrontierWrites
	tx.parent.commits = parentCommits
	tx.parent.rollbacks = parentRollbacks
	return nil
}

func (tx actorTurnCommitTransaction) Rollback(context.Context) error {
	tx.parent.rollbacks++
	return nil
}

type actorTurnCAS struct {
	object cas.Object
	body   []byte
}

func (c actorTurnCAS) Stat(context.Context, string) (cas.Object, error) { return c.object, nil }

func (c actorTurnCAS) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(c.body)), nil
}

func (actorTurnCAS) Put(context.Context, string, io.Reader) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected CAS put")
}

func (actorTurnCAS) Publish(context.Context, string, io.Reader) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected CAS publish")
}

func (actorTurnCAS) Stage(context.Context, string) (cas.Stage, error) {
	return nil, errors.New("unexpected CAS stage")
}

func (actorTurnCAS) Delete(context.Context, string) error {
	return errors.New("unexpected CAS delete")
}

func newActorTurnCommitFixture(t *testing.T) (
	*Server,
	*actorTurnCommitStore,
	workerActor,
	api.WorkerCommitActorTurnRequest,
	parsedActorTurnCommit,
) {
	t.Helper()
	worker, locators, authority := validActorRunLeaseClaimFixture()
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	authority.run.Status = db.RunStatusRunning
	authority.run.StartedAt = pgvalue.Timestamptz(now.Add(-time.Minute))
	authority.run.ActiveStartedAt = pgvalue.Timestamptz(now.Add(-time.Minute))
	authority.run.MaxActiveDurationMs = 300_000
	authority.attempt.EntrypointEnteredAt = pgvalue.Timestamptz(now.Add(-time.Minute))
	authority.runLease.State = db.RunLeaseStateRunning
	authority.runLease.StartedAt = pgvalue.Timestamptz(now.Add(-time.Minute))
	authority.runLease.StartDeadlineAt = pgvalue.Timestamptz(now.Add(time.Minute))
	authority.runLease.ExpiresAt = pgvalue.Timestamptz(now.Add(5 * time.Minute))
	authority.workspaceLease.ExpiresAt = authority.runLease.ExpiresAt
	authority.workspaceMount.WorkspaceID = authority.workspace.ID
	authority.workspaceMount.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.WorkspaceID = authority.workspace.ID
	authority.workspaceLease.WorkspaceMountID = authority.workspaceMount.ID
	authority.workspaceLease.RuntimeInstanceID = authority.runtime.ID
	authority.workspace.DirtyState = db.WorkspaceDirtyStateClean

	projection := runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		networkSlot: authority.networkSlot, runLease: authority.runLease,
		workspace: authority.workspace, workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	}
	receipt, err := projectRunLeaseReceipt(projection)
	if err != nil {
		t.Fatal(err)
	}
	resetTarget := validWorkspaceResetTargetAuthority(projection)
	store := &actorTurnCommitStore{
		runLeaseClaimStore: &runLeaseClaimStore{
			authority: authority,
			renewal: db.GetLiveRunLeaseLocatorsRow{
				OrgID: locators.OrgID, ProjectID: locators.ProjectID,
				EnvironmentID: locators.EnvironmentID, RunID: locators.RunID,
				WorkspaceID: locators.WorkspaceID, AttemptNumber: locators.AttemptNumber,
				ActorID: authority.actor.ID, RegionID: locators.RegionID,
				RuntimeInstanceID: locators.RuntimeInstanceID, NetworkSlotID: locators.NetworkSlotID,
				NetworkSlotGeneration: locators.NetworkSlotGeneration,
				WorkspaceLeaseID:      locators.WorkspaceLeaseID, WorkspaceMountID: locators.WorkspaceMountID,
			},
			finalizationClear: pgtype.Bool{Bool: true, Valid: true},
			resetTarget:       resetTarget,
		},
		committedAt: pgvalue.Timestamptz(now),
	}
	request := api.WorkerCommitActorTurnRequest{
		Lease: receipt, CorrelationID: uuid.Must(uuid.NewV7()).String(),
		TargetInputSequence: 2, BaseWorkspaceVersionID: receipt.BaseWorkspaceVersionID,
		Tree: api.WorkerWorkspaceTreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
	}
	commit, err := parseActorTurnCommitRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{db: store}, store, worker, request, commit
}
