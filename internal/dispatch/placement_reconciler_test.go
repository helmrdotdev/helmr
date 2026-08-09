package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type countingRunPlacementDiscovery struct {
	calls atomic.Int64
	wake  chan struct{}
}

func (f *countingRunPlacementDiscovery) ListOrganizations(context.Context, int16, pgtype.UUID, int32) ([]pgtype.UUID, error) {
	f.calls.Add(1)
	select {
	case f.wake <- struct{}{}:
	default:
	}
	return nil, nil
}
func (*countingRunPlacementDiscovery) ListScopes(context.Context, runPlacementScopeParams) ([]runPlacementScopeRow, error) {
	return nil, nil
}
func (*countingRunPlacementDiscovery) ListCandidates(context.Context, db.ListQueuedRunPlacementCandidatesParams) ([]db.ListQueuedRunPlacementCandidatesRow, error) {
	return nil, nil
}

type blockingBuildPlacementDiscovery struct {
	started chan struct{}
}

func (f blockingBuildPlacementDiscovery) ListQueuedDeploymentBuildRegions(ctx context.Context, _ int32) ([]string, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingBuildPlacementDiscovery) ListQueuedDeploymentBuildCandidates(context.Context, db.ListQueuedDeploymentBuildCandidatesParams) ([]db.ListQueuedDeploymentBuildCandidatesRow, error) {
	return nil, nil
}

type isolationRunAuthority struct{}

func (isolationRunAuthority) PlaceReadyRun(context.Context, ReadyRunCandidate) (ReadyRunPlacement, error) {
	return ReadyRunPlacement{LeaseCreated: true}, nil
}

func (isolationRunAuthority) PlaceWorkspaceExec(context.Context, ReadyWorkspaceExecCandidate) (WorkspaceExecPlacement, error) {
	return WorkspaceExecPlacement{}, nil
}

func (isolationRunAuthority) RecoverWorkspaceExec(context.Context, RecoverableWorkspaceExecCandidate) error {
	return nil
}

func (isolationRunAuthority) FailPendingWorkspaceExec(context.Context, ReadyWorkspaceExecCandidate, string) error {
	return nil
}

type noopWorkspaceExecDiscovery struct{}

func (noopWorkspaceExecDiscovery) ListPendingWorkspaceExecCandidates(context.Context, int32) ([]db.ListPendingWorkspaceExecCandidatesRow, error) {
	return nil, nil
}

func (noopWorkspaceExecDiscovery) ListRecoverableWorkspaceExecCandidates(context.Context, int32) ([]db.ListRecoverableWorkspaceExecCandidatesRow, error) {
	return nil, nil
}

type isolationBuildAuthority struct{}

func (isolationBuildAuthority) PlaceReadyBuild(context.Context, ReadyBuildCandidate) (db.LeaseQueuedDeploymentBuildRow, error) {
	return db.LeaseQueuedDeploymentBuildRow{}, nil
}

type directRunPlacementLaneLocker struct {
	discovery RunPlacementDiscovery
}

func (l directRunPlacementLaneLocker) TryLock(context.Context, int16) (RunPlacementLaneGuard, bool, error) {
	return directRunPlacementLaneGuard(l), true, nil
}

type directRunPlacementLaneGuard struct {
	discovery RunPlacementDiscovery
}

func (g directRunPlacementLaneGuard) Discovery() RunPlacementDiscovery { return g.discovery }
func (directRunPlacementLaneGuard) Unlock() error                      { return nil }

type observedRunPlacementLaneLocker struct {
	discovery RunPlacementDiscovery
	unlocked  chan struct{}
}

func (l observedRunPlacementLaneLocker) TryLock(context.Context, int16) (RunPlacementLaneGuard, bool, error) {
	return observedRunPlacementLaneGuard(l), true, nil
}

type observedRunPlacementLaneGuard struct {
	discovery RunPlacementDiscovery
	unlocked  chan struct{}
}

func (g observedRunPlacementLaneGuard) Discovery() RunPlacementDiscovery { return g.discovery }
func (g observedRunPlacementLaneGuard) Unlock() error {
	select {
	case g.unlocked <- struct{}{}:
	default:
	}
	return nil
}

func TestPlacementReconcilerBlockedBuildDatabaseWorkDoesNotStarveRunPlacement(t *testing.T) {
	runDiscovery := &countingRunPlacementDiscovery{wake: make(chan struct{}, 8)}
	buildStarted := make(chan struct{}, 1)
	reconciler, err := NewPlacementReconciler(
		runDiscovery, directRunPlacementLaneLocker{discovery: runDiscovery}, isolationRunAuthority{},
		blockingBuildPlacementDiscovery{started: buildStarted}, isolationBuildAuthority{},
		noopWorkspaceExecDiscovery{}, isolationRunAuthority{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.runPolicy.idleInterval = 5 * time.Millisecond
	reconciler.runPolicy.failureBackoff = 5 * time.Millisecond
	reconciler.runPolicy.timeout = 100 * time.Millisecond
	reconciler.buildPolicy.interval = time.Hour
	reconciler.buildPolicy.failureBackoff = time.Hour
	reconciler.buildPolicy.timeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	select {
	case <-buildStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("build placement database work did not start")
	}
	for runDiscovery.calls.Load() < 3 {
		select {
		case <-runDiscovery.wake:
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("run placement stalled while build database work was blocked; calls=%d", runDiscovery.calls.Load())
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("placement reconciler did not stop")
	}
}

type unavailableRunPlacementAuthority struct {
	called chan struct{}
}

func (a unavailableRunPlacementAuthority) PlaceReadyRun(context.Context, ReadyRunCandidate) (ReadyRunPlacement, error) {
	select {
	case a.called <- struct{}{}:
	default:
	}
	return ReadyRunPlacement{}, ErrCapacityUnavailable
}

func TestRunPlacementKeepsLaneWhileCoolingKnownUnavailableWork(t *testing.T) {
	organizationID := placementTestUUID(1)
	discovery := testRunPlacementDiscovery{
		organizations: []pgtype.UUID{organizationID},
		scopes: []runPlacementScopeRow{{scope: runPlacementScope{
			orgID: organizationID, environmentID: placementTestUUID(2), queueName: "default",
		}}},
		candidates: map[string][]db.ListQueuedRunPlacementCandidatesRow{
			"default": {{OrgID: organizationID, RunID: placementTestUUID(3)}},
		},
	}
	called := make(chan struct{}, 1)
	unlocked := make(chan struct{}, 1)
	reconciler := &PlacementReconciler{
		runLaneLocker: observedRunPlacementLaneLocker{discovery: discovery, unlocked: unlocked},
		runAuthority:  unavailableRunPlacementAuthority{called: called},
		runPolicy:     testRunPlacementPolicy(1),
		log:           slog.Default(),
	}
	reconciler.runPolicy.idleInterval = 100 * time.Millisecond
	reconciler.runPolicy.timeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.runPlacementLoop(ctx) }()
	select {
	case <-called:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("capacity decision did not run")
	}
	select {
	case <-unlocked:
		cancel()
		t.Fatal("lane was released before its distributed cooldown")
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-unlocked:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("lane was not released after cooldown")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runPlacementLoop() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run placement loop did not stop")
	}
}

func TestRunPlacementOnlyCoolsAfterCompleteOrganizationPass(t *testing.T) {
	first := placementTestUUID(1)
	second := placementTestUUID(2)
	discovery := testRunPlacementDiscovery{
		organizations: []pgtype.UUID{first, second},
		scopes: []runPlacementScopeRow{
			{scope: runPlacementScope{orgID: first, environmentID: placementTestUUID(11), queueName: "first"}},
			{scope: runPlacementScope{orgID: second, environmentID: placementTestUUID(12), queueName: "second"}},
		},
		candidates: map[string][]db.ListQueuedRunPlacementCandidatesRow{
			"first":  {{OrgID: first, RunID: placementTestUUID(21)}},
			"second": {{OrgID: second, RunID: placementTestUUID(22)}},
		},
	}
	reconciler := &PlacementReconciler{
		runAuthority: unavailableRunPlacementAuthority{},
		runPolicy:    testRunPlacementPolicy(1),
	}
	firstBatch, err := reconciler.reconcileRunLane(t.Context(), 0, discovery)
	if err != nil {
		t.Fatal(err)
	}
	if !firstBatch.capacityBlocked() || firstBatch.completedOrganizationPass {
		t.Fatalf("first batch = %+v, want blocked before the Organization pass completes", firstBatch)
	}
	secondBatch, err := reconciler.reconcileRunLane(t.Context(), 0, discovery)
	if err != nil {
		t.Fatal(err)
	}
	if !secondBatch.capacityBlocked() || !secondBatch.completedOrganizationPass {
		t.Fatalf("second batch = %+v, want blocked at the completed Organization pass", secondBatch)
	}
}

type testRunPlacementDiscovery struct {
	organizations []pgtype.UUID
	scopes        []runPlacementScopeRow
	candidates    map[string][]db.ListQueuedRunPlacementCandidatesRow
}

func (f testRunPlacementDiscovery) ListOrganizations(_ context.Context, _ int16, after pgtype.UUID, limit int32) ([]pgtype.UUID, error) {
	result := make([]pgtype.UUID, 0, min(len(f.organizations), int(limit)))
	for _, orgID := range f.organizations {
		if after.Valid && bytes.Compare(orgID.Bytes[:], after.Bytes[:]) <= 0 {
			continue
		}
		result = append(result, orgID)
		if len(result) == int(limit) {
			break
		}
	}
	return result, nil
}

func (f testRunPlacementDiscovery) ListScopes(_ context.Context, arg runPlacementScopeParams) ([]runPlacementScopeRow, error) {
	byOrganization := make([][]runPlacementScope, len(arg.organizations))
	for i, orgID := range arg.organizations {
		for _, row := range f.scopes {
			if row.scope.orgID != orgID || !runPlacementScopeAfter(row.scope, arg.after[i]) {
				continue
			}
			byOrganization[i] = append(byOrganization[i], row.scope)
			if len(byOrganization[i]) == int(arg.limit) {
				break
			}
		}
	}
	var result []runPlacementScopeRow
	for position := 0; ; position++ {
		added := false
		for i, scopes := range byOrganization {
			if position >= len(scopes) {
				continue
			}
			added = true
			result = append(result, runPlacementScopeRow{
				organizationOrdinal: int64(i + 1),
				scope:               scopes[position],
			})
		}
		if !added {
			return result, nil
		}
	}
}

func (f testRunPlacementDiscovery) ListCandidates(_ context.Context, arg db.ListQueuedRunPlacementCandidatesParams) ([]db.ListQueuedRunPlacementCandidatesRow, error) {
	var result []db.ListQueuedRunPlacementCandidatesRow
	for index, queue := range arg.QueueNames {
		count := int32(0)
		for _, row := range f.candidates[queue] {
			if arg.AfterSet[index] && bytes.Compare(row.RunID.Bytes[:], arg.AfterRunIds[index].Bytes[:]) <= 0 {
				continue
			}
			row.ScopeOrdinal = int64(index + 1)
			result = append(result, row)
			count++
			if count == arg.CandidateLimits[index] {
				break
			}
		}
	}
	return result, nil
}

func runPlacementScopeAfter(scope runPlacementScope, after runPlacementScopeCursor) bool {
	if !after.set {
		return true
	}
	if compared := bytes.Compare(scope.environmentID.Bytes[:], after.environmentID.Bytes[:]); compared != 0 {
		return compared > 0
	}
	if scope.queueName != after.queueName {
		return scope.queueName > after.queueName
	}
	return scope.concurrencyKey > after.concurrencyKey
}

type recordingRunPlacementAuthority struct {
	placed []byte
}

func (a *recordingRunPlacementAuthority) PlaceReadyRun(_ context.Context, candidate ReadyRunCandidate) (ReadyRunPlacement, error) {
	a.placed = append(a.placed, candidate.RunID.Bytes[15])
	return ReadyRunPlacement{LeaseCreated: true}, nil
}

func TestPlacementReconcilerAcceptsNoEligibleRuns(t *testing.T) {
	reconciler := &PlacementReconciler{
		runDiscovery: testRunPlacementDiscovery{},
		runAuthority: &recordingRunPlacementAuthority{},
		runPolicy:    testRunPlacementPolicy(32),
	}
	if err := reconciler.ReconcileRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type parallelRunPlacementAuthority struct {
	active  atomic.Int64
	maximum atomic.Int64
	placed  atomic.Int64
	release chan struct{}
	once    sync.Once
}

func (a *parallelRunPlacementAuthority) PlaceReadyRun(
	ctx context.Context,
	_ ReadyRunCandidate,
) (ReadyRunPlacement, error) {
	active := a.active.Add(1)
	defer a.active.Add(-1)
	for {
		maximum := a.maximum.Load()
		if active <= maximum || a.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	if active == 2 {
		a.once.Do(func() { close(a.release) })
	}
	select {
	case <-a.release:
	case <-ctx.Done():
		return ReadyRunPlacement{}, ctx.Err()
	}
	a.placed.Add(1)
	return ReadyRunPlacement{LeaseCreated: true}, nil
}

func TestPlacementReconcilerBoundsParallelRunPlacement(t *testing.T) {
	authority := &parallelRunPlacementAuthority{release: make(chan struct{})}
	reconciler := &PlacementReconciler{
		runAuthority: authority,
		runParallel:  make(chan struct{}, 2),
	}
	work := make([]runPlacementWork, 8)
	for index := range work {
		work[index].candidate = db.ListQueuedRunPlacementCandidatesRow{
			OrgID: placementTestUUID(byte(index + 1)), RunID: placementTestUUID(byte(index + 11)),
		}
	}
	batch, _, err := reconciler.placeRunCandidates(t.Context(), work)
	if err != nil {
		t.Fatal(err)
	}
	if batch.placed != len(work) || authority.placed.Load() != int64(len(work)) {
		t.Fatalf("placed = %d/%d, want %d", batch.placed, authority.placed.Load(), len(work))
	}
	if maximum := authority.maximum.Load(); maximum != 2 {
		t.Fatalf("maximum concurrent placements = %d, want 2", maximum)
	}
}

func TestPlacementReconcilerInterleavesScopesWithinOrganization(t *testing.T) {
	authority := &recordingRunPlacementAuthority{}
	reconciler := &PlacementReconciler{
		runDiscovery: testRunPlacementDiscovery{
			organizations: []pgtype.UUID{placementTestUUID(1)},
			scopes: []runPlacementScopeRow{
				{organizationOrdinal: 1, scope: runPlacementScope{orgID: placementTestUUID(1), environmentID: placementTestUUID(31), queueName: "a"}},
				{organizationOrdinal: 1, scope: runPlacementScope{orgID: placementTestUUID(1), environmentID: placementTestUUID(32), queueName: "b"}},
			},
			candidates: map[string][]db.ListQueuedRunPlacementCandidatesRow{
				"a": {{OrgID: placementTestUUID(1), RunID: placementTestUUID(11)}, {OrgID: placementTestUUID(1), RunID: placementTestUUID(12)}},
				"b": {{OrgID: placementTestUUID(1), RunID: placementTestUUID(21)}, {OrgID: placementTestUUID(1), RunID: placementTestUUID(22)}},
			},
		},
		runAuthority: authority,
		runPolicy:    testRunPlacementPolicy(4),
	}
	if err := reconciler.ReconcileRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []byte{11, 21, 12, 22}
	if len(authority.placed) != len(want) {
		t.Fatalf("placed = %v, want %v", authority.placed, want)
	}
	for index := range want {
		if authority.placed[index] != want[index] {
			t.Fatalf("placed = %v, want %v", authority.placed, want)
		}
	}
}

type pendingRunPlacementAuthority struct {
	calls []byte
}

func (a *pendingRunPlacementAuthority) PlaceReadyRun(
	_ context.Context,
	candidate ReadyRunCandidate,
) (ReadyRunPlacement, error) {
	a.calls = append(a.calls, candidate.RunID.Bytes[15])
	if len(a.calls) == 1 {
		return ReadyRunPlacement{}, nil
	}
	return ReadyRunPlacement{LeaseCreated: true}, nil
}

func TestPlacementReconcilerKeepsPendingRunAtScopeHead(t *testing.T) {
	orgID := placementTestUUID(1)
	scope := runPlacementScope{orgID: orgID, environmentID: placementTestUUID(31), queueName: "queue"}
	authority := &pendingRunPlacementAuthority{}
	policy := testRunPlacementPolicy(4)
	policy.pendingInterval = time.Hour
	reconciler := &PlacementReconciler{
		runDiscovery: testRunPlacementDiscovery{
			organizations: []pgtype.UUID{orgID},
			scopes:        []runPlacementScopeRow{{organizationOrdinal: 1, scope: scope}},
			candidates: map[string][]db.ListQueuedRunPlacementCandidatesRow{
				"queue": {
					{OrgID: orgID, RunID: placementTestUUID(11)},
					{OrgID: orgID, RunID: placementTestUUID(12)},
					{OrgID: orgID, RunID: placementTestUUID(13)},
				},
			},
		},
		runAuthority: authority,
		runPolicy:    policy,
	}
	if err := reconciler.ReconcileRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := authority.calls; len(got) != 1 || got[0] != 11 {
		t.Fatalf("placement calls after pending result = %v, want [11]", got)
	}
	if err := reconciler.ReconcileRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := authority.calls; len(got) != 1 {
		t.Fatalf("placement calls during pending cooldown = %v, want [11]", got)
	}
	state := reconciler.runCursors[0].organization(orgID).candidates[scope]
	state.pendingUntil = time.Now().Add(-time.Second)
	reconciler.runCursors[0].organization(orgID).candidates[scope] = state
	if err := reconciler.ReconcileRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := authority.calls; len(got) < 2 || got[0] != 11 || got[1] != 11 {
		t.Fatalf("placement calls after pending clears = %v, want pending Run retried first", got)
	}
}

type longPendingRunPlacementAuthority struct {
	calls []byte
}

func (a *longPendingRunPlacementAuthority) PlaceReadyRun(
	_ context.Context,
	candidate ReadyRunCandidate,
) (ReadyRunPlacement, error) {
	a.calls = append(a.calls, candidate.RunID.Bytes[15])
	if candidate.RunID.Bytes[15] == 11 {
		return ReadyRunPlacement{}, nil
	}
	return ReadyRunPlacement{LeaseCreated: true}, nil
}

func TestPlacementReconcilerPendingScopeDoesNotBlockLaterScopes(t *testing.T) {
	orgID := placementTestUUID(1)
	discovery := testRunPlacementDiscovery{
		organizations: []pgtype.UUID{orgID},
		candidates:    make(map[string][]db.ListQueuedRunPlacementCandidatesRow),
	}
	var pendingScope runPlacementScope
	for index := range 34 {
		queue := fmt.Sprintf("queue-%02d", index)
		scope := runPlacementScope{
			orgID: orgID, environmentID: placementTestUUID(byte(31 + index)), queueName: queue,
		}
		if index == 0 {
			pendingScope = scope
		}
		discovery.scopes = append(discovery.scopes, runPlacementScopeRow{
			organizationOrdinal: 1, scope: scope,
		})
		discovery.candidates[queue] = []db.ListQueuedRunPlacementCandidatesRow{{
			OrgID: orgID, RunID: placementTestUUID(byte(11 + index)),
		}}
	}
	authority := &longPendingRunPlacementAuthority{}
	policy := testRunPlacementPolicy(32)
	policy.pendingInterval = time.Hour
	reconciler := &PlacementReconciler{
		runDiscovery: discovery,
		runAuthority: authority,
		runPolicy:    policy,
	}
	if err := reconciler.ReconcileRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(authority.calls, byte(43)) {
		t.Fatal("scope 33 was attempted before the first bounded scope page completed")
	}
	if err := reconciler.ReconcileRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(authority.calls, byte(43)) {
		t.Fatalf("later scope was not attempted while the first scope remained pending: %v", authority.calls)
	}
	state := reconciler.runCursors[0].organization(orgID).candidates[pendingScope]
	state.pendingUntil = time.Now().Add(-time.Second)
	reconciler.runCursors[0].organization(orgID).candidates[pendingScope] = state
	before := len(authority.calls)
	if err := reconciler.ReconcileRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(authority.calls[before:], byte(11)) {
		t.Fatalf("pending scope was not retried after cooldown: %v", authority.calls[before:])
	}
}

type failingHeadRunPlacementAuthority struct {
	calls []byte
}

func (a *failingHeadRunPlacementAuthority) PlaceReadyRun(
	_ context.Context,
	candidate ReadyRunCandidate,
) (ReadyRunPlacement, error) {
	a.calls = append(a.calls, candidate.RunID.Bytes[15])
	if candidate.RunID.Bytes[15] == 11 {
		return ReadyRunPlacement{}, errors.New("placement unavailable")
	}
	return ReadyRunPlacement{LeaseCreated: true}, nil
}

func TestPlacementReconcilerFailureDoesNotAdvancePastUnattemptedScopes(t *testing.T) {
	orgID := placementTestUUID(1)
	discovery := testRunPlacementDiscovery{
		organizations: []pgtype.UUID{orgID},
		candidates:    make(map[string][]db.ListQueuedRunPlacementCandidatesRow),
	}
	for index := range 34 {
		queue := fmt.Sprintf("queue-%02d", index)
		discovery.scopes = append(discovery.scopes, runPlacementScopeRow{
			organizationOrdinal: 1,
			scope: runPlacementScope{
				orgID: orgID, environmentID: placementTestUUID(byte(31 + index)), queueName: queue,
			},
		})
		discovery.candidates[queue] = []db.ListQueuedRunPlacementCandidatesRow{{
			OrgID: orgID, RunID: placementTestUUID(byte(11 + index)),
		}}
	}
	authority := &failingHeadRunPlacementAuthority{}
	policy := testRunPlacementPolicy(32)
	policy.pendingInterval = time.Hour
	reconciler := &PlacementReconciler{
		runDiscovery: discovery,
		runAuthority: authority,
		runPolicy:    policy,
	}
	if err := reconciler.ReconcileRuns(context.Background()); err == nil {
		t.Fatal("first placement cycle succeeded despite the injected failure")
	}
	if got := authority.calls; len(got) != 1 || got[0] != 11 {
		t.Fatalf("first placement calls = %v, want only the failing head", got)
	}
	if err := reconciler.ReconcileRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(authority.calls, byte(43)) {
		t.Fatalf("unattempted scopes were skipped after the first-round failure: %v", authority.calls)
	}
}

type organizationCountingRunPlacementAuthority struct {
	placed map[byte]int
}

func (a *organizationCountingRunPlacementAuthority) PlaceReadyRun(
	_ context.Context,
	candidate ReadyRunCandidate,
) (ReadyRunPlacement, error) {
	a.placed[candidate.OrgID.Bytes[15]]++
	return ReadyRunPlacement{LeaseCreated: true}, nil
}

func TestPlacementReconcilerSharesAttemptsAcrossOrganizations(t *testing.T) {
	tests := []struct {
		name       string
		secondRuns int
		wantFirst  int
		wantSecond int
	}{
		{name: "deep backlogs", secondRuns: 32, wantFirst: 16, wantSecond: 16},
		{name: "unused share", secondRuns: 1, wantFirst: 31, wantSecond: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			discovery := skewedOrganizationRunPlacementDiscovery(test.secondRuns)
			authority := &organizationCountingRunPlacementAuthority{placed: make(map[byte]int)}
			reconciler := &PlacementReconciler{
				runDiscovery: discovery,
				runAuthority: authority,
				runPolicy:    testRunPlacementPolicy(32),
			}
			if err := reconciler.ReconcileRuns(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := authority.placed[1]; got != test.wantFirst {
				t.Fatalf("first organization placements = %d, want %d", got, test.wantFirst)
			}
			if got := authority.placed[2]; got != test.wantSecond {
				t.Fatalf("second organization placements = %d, want %d", got, test.wantSecond)
			}
		})
	}
}

type distinctRunPlacementAuthority struct {
	placed map[byte]struct{}
}

func (a *distinctRunPlacementAuthority) PlaceReadyRun(
	_ context.Context,
	candidate ReadyRunCandidate,
) (ReadyRunPlacement, error) {
	if candidate.OrgID.Bytes[15] == 1 {
		a.placed[candidate.RunID.Bytes[15]] = struct{}{}
	}
	return ReadyRunPlacement{LeaseCreated: true}, nil
}

func TestPlacementReconcilerAdvancesEverySelectedOrganizationScope(t *testing.T) {
	authority := &distinctRunPlacementAuthority{placed: make(map[byte]struct{})}
	reconciler := &PlacementReconciler{
		runDiscovery: skewedOrganizationRunPlacementDiscovery(64),
		runAuthority: authority,
		runPolicy:    testRunPlacementPolicy(32),
	}
	for range 2 {
		if err := reconciler.ReconcileRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(authority.placed); got != 31 {
		t.Fatalf("distinct first organization scopes placed after two cycles = %d, want 31", got)
	}
}

func skewedOrganizationRunPlacementDiscovery(secondRuns int) testRunPlacementDiscovery {
	firstOrganization := placementTestUUID(1)
	secondOrganization := placementTestUUID(2)
	discovery := testRunPlacementDiscovery{
		organizations: []pgtype.UUID{firstOrganization, secondOrganization},
		candidates:    make(map[string][]db.ListQueuedRunPlacementCandidatesRow),
	}
	for index := range 31 {
		queue := fmt.Sprintf("first-%02d", index)
		discovery.scopes = append(discovery.scopes, runPlacementScopeRow{
			organizationOrdinal: 1,
			scope: runPlacementScope{
				orgID: firstOrganization, environmentID: placementTestUUID(byte(31 + index)), queueName: queue,
			},
		})
		discovery.candidates[queue] = []db.ListQueuedRunPlacementCandidatesRow{{
			OrgID: firstOrganization, RunID: placementTestUUID(byte(31 + index)),
		}}
		if index == 0 {
			discovery.scopes = append(discovery.scopes, runPlacementScopeRow{
				organizationOrdinal: 2,
				scope: runPlacementScope{
					orgID: secondOrganization, environmentID: placementTestUUID(30), queueName: "second",
				},
			})
		}
	}
	for index := range secondRuns {
		discovery.candidates["second"] = append(
			discovery.candidates["second"],
			db.ListQueuedRunPlacementCandidatesRow{
				OrgID: secondOrganization, RunID: placementTestUUID(byte(100 + index)),
			},
		)
	}
	return discovery
}

func placementTestUUID(last byte) pgtype.UUID {
	value := pgtype.UUID{Valid: true}
	value.Bytes[15] = last
	return value
}

func testRunPlacementPolicy(limit int32) runPlacementPolicy {
	return runPlacementPolicy{
		idleInterval: time.Second, failureBackoff: time.Second, timeout: time.Second,
		workers: 1, organizationLimit: limit, attemptLimit: limit, parallelism: 1,
	}
}
