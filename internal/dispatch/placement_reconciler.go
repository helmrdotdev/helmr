package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultRunPlacementIdleInterval      = time.Second
	defaultRunPlacementFailureBackoff    = time.Second
	defaultRunPlacementTimeout           = 15 * time.Second
	defaultRunPlacementWorkers           = 8
	defaultRunPlacementOrganizationLimit = int32(32)
	defaultRunPlacementAttemptLimit      = int32(32)
	defaultRunPlacementParallelism       = 16
	defaultRunPlacementPendingInterval   = time.Second
	defaultBuildPlacementInterval        = 2 * time.Second
	defaultBuildPlacementFailureBackoff  = 5 * time.Second
	defaultBuildPlacementTimeout         = 30 * time.Second
	defaultBuildPlacementLimit           = int32(8)
	defaultWorkspaceExecPlacementLimit   = int32(32)
	defaultWorkspaceExecPendingTimeout   = 10 * time.Minute
)

type RunPlacementDiscovery interface {
	ListOrganizations(context.Context, int16, pgtype.UUID, int32) ([]pgtype.UUID, error)
	ListScopes(context.Context, runPlacementScopeParams) ([]runPlacementScopeRow, error)
	ListCandidates(context.Context, db.ListQueuedRunPlacementCandidatesParams) ([]db.ListQueuedRunPlacementCandidatesRow, error)
}

type BuildPlacementDiscovery interface {
	ListQueuedDeploymentBuildCandidates(context.Context, db.ListQueuedDeploymentBuildCandidatesParams) ([]db.ListQueuedDeploymentBuildCandidatesRow, error)
	ListQueuedDeploymentBuildRegions(context.Context, int32) ([]string, error)
}

type RunPlacementAuthority interface {
	PlaceReadyRun(context.Context, ReadyRunCandidate) (ReadyRunPlacement, error)
}

type BuildPlacementAuthority interface {
	PlaceReadyBuild(context.Context, ReadyBuildCandidate) (db.LeaseQueuedDeploymentBuildRow, error)
}

type WorkspaceExecPlacementDiscovery interface {
	ListPendingWorkspaceExecCandidates(
		context.Context,
		int32,
	) ([]db.ListPendingWorkspaceExecCandidatesRow, error)
	ListRecoverableWorkspaceExecCandidates(
		context.Context,
		int32,
	) ([]db.ListRecoverableWorkspaceExecCandidatesRow, error)
}

type WorkspaceExecPlacementAuthority interface {
	PlaceWorkspaceExec(
		context.Context,
		ReadyWorkspaceExecCandidate,
	) (WorkspaceExecPlacement, error)
	RecoverWorkspaceExec(
		context.Context,
		RecoverableWorkspaceExecCandidate,
	) error
	FailPendingWorkspaceExec(
		context.Context,
		ReadyWorkspaceExecCandidate,
		string,
	) error
}

type PlacementReconciler struct {
	runDiscovery           RunPlacementDiscovery
	runLaneLocker          RunPlacementLaneLocker
	runAuthority           RunPlacementAuthority
	buildDiscovery         BuildPlacementDiscovery
	buildAuthority         BuildPlacementAuthority
	workspaceExecDiscovery WorkspaceExecPlacementDiscovery
	workspaceExecAuthority WorkspaceExecPlacementAuthority
	runPolicy              runPlacementPolicy
	buildPolicy            placementLoopPolicy
	workspaceExecPolicy    placementLoopPolicy
	runCursors             [runPlacementLaneCount]runPlacementCursor
	runLaneMutexes         [runPlacementLaneCount]sync.Mutex
	runNextLane            atomic.Uint32
	runParallel            chan struct{}
	metrics                reconcileMetrics
	log                    *slog.Logger
}

type placementLoopPolicy struct {
	interval       time.Duration
	failureBackoff time.Duration
	timeout        time.Duration
	limit          int32
}

type runPlacementPolicy struct {
	idleInterval      time.Duration
	failureBackoff    time.Duration
	timeout           time.Duration
	workers           int
	organizationLimit int32
	attemptLimit      int32
	parallelism       int
	pendingInterval   time.Duration
}

type runPlacementOutcome uint8

const (
	runPlacementPlaced runPlacementOutcome = iota
	runPlacementPending
	runPlacementChanged
	runPlacementUnavailable
)

type runPlacementBatch struct {
	attempted                 int
	placed                    int
	pending                   int
	changed                   int
	unavailable               int
	completedOrganizationPass bool
}

type runPlacementResult struct {
	work    runPlacementWork
	outcome runPlacementOutcome
	err     error
}

type runPlacementWork struct {
	scope     runPlacementScope
	candidate db.ListQueuedRunPlacementCandidatesRow
	end       bool
	examined  int
}

func (b runPlacementBatch) capacityBlocked() bool {
	return b.attempted > 0 && b.unavailable == b.attempted
}

func (b *runPlacementBatch) add(page runPlacementBatch) {
	b.attempted += page.attempted
	b.placed += page.placed
	b.pending += page.pending
	b.changed += page.changed
	b.unavailable += page.unavailable
	b.completedOrganizationPass = b.completedOrganizationPass || page.completedOrganizationPass
}

type runPlacementScopeCandidates struct {
	scope runPlacementScope
	rows  []db.ListQueuedRunPlacementCandidatesRow
	next  int
	limit int
}

type runPlacementOrganizationCandidates struct {
	scopes   []runPlacementScopeCandidates
	next     int
	seen     int
	examined int
	end      bool
}

func (c *runPlacementOrganizationCandidates) take(
	blocked map[runPlacementScope]struct{},
) (runPlacementScope, db.ListQueuedRunPlacementCandidatesRow, bool, int, bool) {
	for range len(c.scopes) {
		index := c.next
		c.next = (c.next + 1) % len(c.scopes)
		if c.seen < len(c.scopes) {
			c.seen++
		}
		scope := &c.scopes[index]
		if _, skip := blocked[scope.scope]; skip {
			continue
		}
		if scope.next >= len(scope.rows) {
			continue
		}
		row := scope.rows[scope.next]
		scope.next++
		end := len(scope.rows) < scope.limit && scope.next == len(scope.rows)
		return scope.scope, row, end, c.seen, true
	}
	return runPlacementScope{}, db.ListQueuedRunPlacementCandidatesRow{}, false, c.seen, false
}

func NewPlacementReconciler(runDiscovery RunPlacementDiscovery, runLaneLocker RunPlacementLaneLocker,
	runAuthority RunPlacementAuthority,
	buildDiscovery BuildPlacementDiscovery, buildAuthority BuildPlacementAuthority,
	workspaceExecDiscovery WorkspaceExecPlacementDiscovery,
	workspaceExecAuthority WorkspaceExecPlacementAuthority,
	log *slog.Logger,
) (*PlacementReconciler, error) {
	if runDiscovery == nil || runLaneLocker == nil || runAuthority == nil || buildDiscovery == nil || buildAuthority == nil ||
		workspaceExecDiscovery == nil || workspaceExecAuthority == nil {
		return nil, errors.New("run, build, and workspace exec placement dependencies are required")
	}
	if log == nil {
		log = slog.Default()
	}
	reconciler := &PlacementReconciler{
		runDiscovery: runDiscovery, runLaneLocker: runLaneLocker, runAuthority: runAuthority,
		buildDiscovery: buildDiscovery, buildAuthority: buildAuthority,
		workspaceExecDiscovery: workspaceExecDiscovery,
		workspaceExecAuthority: workspaceExecAuthority,
		log:                    log, metrics: newReconcileMetrics(),
		runPolicy: runPlacementPolicy{
			idleInterval:      defaultRunPlacementIdleInterval,
			failureBackoff:    defaultRunPlacementFailureBackoff,
			timeout:           defaultRunPlacementTimeout,
			workers:           defaultRunPlacementWorkers,
			organizationLimit: defaultRunPlacementOrganizationLimit,
			attemptLimit:      defaultRunPlacementAttemptLimit,
			parallelism:       defaultRunPlacementParallelism,
			pendingInterval:   defaultRunPlacementPendingInterval,
		},
		buildPolicy: placementLoopPolicy{
			interval: defaultBuildPlacementInterval, failureBackoff: defaultBuildPlacementFailureBackoff,
			timeout: defaultBuildPlacementTimeout, limit: defaultBuildPlacementLimit,
		},
		workspaceExecPolicy: placementLoopPolicy{
			interval: defaultRunPlacementIdleInterval, failureBackoff: defaultRunPlacementFailureBackoff,
			timeout: defaultRunPlacementTimeout, limit: defaultWorkspaceExecPlacementLimit,
		},
	}
	reconciler.runParallel = make(chan struct{}, reconciler.runPolicy.parallelism)
	return reconciler, nil
}

func (r *PlacementReconciler) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	loops := r.runPolicy.workers + 2
	errC := make(chan error, loops)
	for range r.runPolicy.workers {
		go func() { errC <- r.runPlacementLoop(runCtx) }()
	}
	go func() { errC <- r.runLoop(runCtx, "build", r.buildPolicy, r.ReconcileBuilds) }()
	go func() {
		errC <- r.runLoop(
			runCtx,
			"workspace_exec",
			r.workspaceExecPolicy,
			r.ReconcileWorkspaceExecs,
		)
	}()
	var firstErr error
	for i := range loops {
		err := <-errC
		if firstErr == nil && err != nil && !errors.Is(err, context.Canceled) {
			firstErr = err
		}
		if i == 0 {
			cancel()
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (r *PlacementReconciler) runPlacementLoop(ctx context.Context) error {
	idleLanes := 0
	for {
		lane := int16((r.runNextLane.Add(1) - 1) % uint32(runPlacementLaneCount))
		started := time.Now()
		cycleCtx, cancel := context.WithTimeout(ctx, r.runPolicy.timeout)
		guard, locked, err := r.runLaneLocker.TryLock(cycleCtx, lane)
		if err != nil {
			cancel()
			r.metrics.observe(ctx, "placement", "run", "failure", time.Since(started))
			r.log.Warn("run placement lane failed", "duration_ms", time.Since(started).Milliseconds(), "error", err)
			if err := waitFor(ctx, r.runPolicy.failureBackoff); err != nil {
				return err
			}
			continue
		}
		if !locked {
			cancel()
			idleLanes++
			if err := r.waitAfterRunLane(ctx, &idleLanes); err != nil {
				return err
			}
			continue
		}

		batch, reconcileErr := r.reconcileRunLane(cycleCtx, lane, guard.Discovery())
		if reconcileErr == nil && batch.capacityBlocked() && batch.completedOrganizationPass {
			// After a complete Organization pass, keep the lane guard while cooling
			// down so another dispatcher cannot immediately repeat the same known-
			// unplaceable work. The guard and delay are bounded and carry no durable
			// scheduling state.
			reconcileErr = waitFor(ctx, r.runPolicy.idleInterval)
		}
		unlockErr := guard.Unlock()
		cancel()
		err = errors.Join(reconcileErr, unlockErr)
		outcome := "success"
		if err != nil {
			outcome = "failure"
			r.log.Warn("run placement reconciliation failed", "duration_ms", time.Since(started).Milliseconds(), "error", err)
		}
		r.metrics.observe(ctx, "placement", "run", outcome, time.Since(started))
		r.metrics.observeRunBatch(ctx, batch)
		if err != nil {
			if err := waitFor(ctx, r.runPolicy.failureBackoff); err != nil {
				return err
			}
			continue
		}
		if batch.attempted == 0 {
			idleLanes++
		} else {
			idleLanes = 0
		}
		if err := r.waitAfterRunLane(ctx, &idleLanes); err != nil {
			return err
		}
	}
}

func (r *PlacementReconciler) waitAfterRunLane(ctx context.Context, idleLanes *int) error {
	workers := max(1, r.runPolicy.workers)
	idleLimit := (runPlacementLaneCount + workers - 1) / workers
	if *idleLanes < idleLimit {
		return nil
	}
	*idleLanes = 0
	return waitFor(ctx, r.runPolicy.idleInterval)
}

func waitFor(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *PlacementReconciler) ReconcileWorkspaceExecs(ctx context.Context) error {
	recoverable, err := r.workspaceExecDiscovery.ListRecoverableWorkspaceExecCandidates(
		ctx,
		r.workspaceExecPolicy.limit,
	)
	if err != nil {
		return fmt.Errorf("list recoverable workspace execs: %w", err)
	}
	var problems []error
	for _, row := range recoverable {
		err := r.workspaceExecAuthority.RecoverWorkspaceExec(
			ctx,
			RecoverableWorkspaceExecCandidate{
				OrgID:                row.OrgID,
				ProcessID:            row.ID,
				WorkspaceID:          row.WorkspaceID,
				ExpectedStateVersion: row.StateVersion,
			},
		)
		if errors.Is(err, ErrCandidateChanged) || errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			problems = append(problems, err)
		}
	}
	rows, err := r.workspaceExecDiscovery.ListPendingWorkspaceExecCandidates(
		ctx,
		r.workspaceExecPolicy.limit,
	)
	if err != nil {
		return fmt.Errorf("list pending workspace execs: %w", err)
	}
	expiredBefore := time.Now().UTC().Add(-defaultWorkspaceExecPendingTimeout)
	for _, row := range rows {
		candidate := ReadyWorkspaceExecCandidate{
			OrgID:                row.OrgID,
			ProcessID:            row.ID,
			ExpectedStateVersion: row.StateVersion,
		}
		if !row.CreatedAt.Time.After(expiredBefore) {
			err := r.workspaceExecAuthority.FailPendingWorkspaceExec(
				ctx,
				candidate,
				"workspace_exec_placement_timed_out",
			)
			if errors.Is(err, ErrCandidateChanged) || errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				problems = append(problems, err)
			}
			continue
		}
		_, err := r.workspaceExecAuthority.PlaceWorkspaceExec(
			ctx,
			candidate,
		)
		if err != nil {
			if errors.Is(err, ErrCandidateChanged) ||
				errors.Is(err, ErrCapacityUnavailable) ||
				errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			problems = append(problems, err)
			continue
		}
	}
	return errors.Join(problems...)
}

func (r *PlacementReconciler) runLoop(ctx context.Context, domain string, policy placementLoopPolicy, reconcile func(context.Context) error) error {
	for {
		started := time.Now()
		cycleCtx, cancel := context.WithTimeout(ctx, policy.timeout)
		err := reconcile(cycleCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		outcome := "success"
		delay := policy.interval
		if err != nil && !errors.Is(err, context.Canceled) {
			outcome = "failure"
			delay = policy.failureBackoff
			r.log.Warn("placement reconciliation failed", "domain", domain, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		}
		r.metrics.observe(ctx, "placement", domain, outcome, time.Since(started))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *PlacementReconciler) ReconcileRuns(ctx context.Context) error {
	_, err := r.reconcileRunLane(ctx, 0, r.runDiscovery)
	return err
}

func (r *PlacementReconciler) reconcileRunLane(
	ctx context.Context,
	lane int16,
	discovery RunPlacementDiscovery,
) (runPlacementBatch, error) {
	if lane < 0 || lane >= runPlacementLaneCount {
		return runPlacementBatch{}, errors.New("run placement lane is out of range")
	}
	if discovery == nil {
		return runPlacementBatch{}, errors.New("run placement discovery is required")
	}
	r.runLaneMutexes[lane].Lock()
	defer r.runLaneMutexes[lane].Unlock()
	r.runCursors[lane].beginCycle()
	remaining := r.runPolicy.attemptLimit
	var batch runPlacementBatch
	var problems []error
	for remaining > 0 {
		page, err := r.reconcileRunLanePage(ctx, lane, discovery, remaining)
		batch.add(page)
		if err != nil {
			problems = append(problems, err)
			break
		}
		if page.attempted == 0 {
			break
		}
		remaining -= int32(page.attempted)
	}
	return batch, errors.Join(problems...)
}

func (r *PlacementReconciler) reconcileRunLanePage(
	ctx context.Context,
	lane int16,
	discovery RunPlacementDiscovery,
	attemptLimit int32,
) (runPlacementBatch, error) {
	cursor := &r.runCursors[lane]
	remaining := attemptLimit
	var batch runPlacementBatch
	var problems []error

	pendingLimit := int(attemptLimit) / 2
	pending, err := r.reconcilePendingRunScopes(ctx, discovery, cursor, pendingLimit)
	batch.add(pending)
	remaining -= int32(pending.attempted)
	if err != nil {
		problems = append(problems, err)
	}
	if remaining <= 0 {
		return batch, errors.Join(problems...)
	}

	organizationFetchLimit := r.runPolicy.organizationLimit + 1
	rows, err := discovery.ListOrganizations(ctx, lane, cursor.afterOrganization, organizationFetchLimit)
	if err != nil {
		problems = append(problems, fmt.Errorf("list run placement organizations: %w", err))
		return batch, errors.Join(problems...)
	}
	organizations := cursor.chooseOrganizations(rows, int(r.runPolicy.organizationLimit))
	if len(organizations) == 0 {
		return batch, errors.Join(problems...)
	}
	scopeShare := (int(remaining) + len(organizations) - 1) / len(organizations)
	scopeFetchLimit := int32(min(scopeShare, runPlacementCandidateScopeLimit) + 1)
	scopeRows, err := discovery.ListScopes(
		ctx,
		cursor.scopeParams(organizations, scopeFetchLimit),
	)
	if err != nil {
		problems = append(problems, fmt.Errorf("list run placement scopes: %w", err))
		return batch, errors.Join(problems...)
	}
	scopeLimit := min(int(remaining), runPlacementCandidateScopeLimit)
	scopes, ends := cursor.chooseScopes(scopeRows, organizations, scopeLimit, int(scopeFetchLimit))
	if len(scopes) == 0 {
		batch.completedOrganizationPass = len(rows) <= int(r.runPolicy.organizationLimit)
		return batch, errors.Join(problems...)
	}
	rowsByScope := make([][]db.ListQueuedRunPlacementCandidatesRow, len(scopes))
	queriedScopes := cursor.readyCandidateScopes(scopes)
	scopesPerOrganization := make(map[pgtype.UUID]int, len(organizations))
	for _, scope := range scopes {
		scopesPerOrganization[scope.orgID]++
	}
	scopeCandidateLimits := make(map[runPlacementScope]int32, len(scopes))
	for _, scope := range scopes {
		count := scopesPerOrganization[scope.orgID]
		limit := (scopeShare + count - 1) / count
		scopeCandidateLimits[scope] = int32(min(limit, runPlacementCandidateScopeLimit))
	}
	queried := make(map[runPlacementScope]struct{}, len(queriedScopes))
	scopeIndexes := make(map[runPlacementScope]int, len(scopes))
	for index, scope := range scopes {
		scopeIndexes[scope] = index
	}
	if len(queriedScopes) > 0 {
		limits := make([]int32, 0, len(queriedScopes))
		for _, scope := range queriedScopes {
			limits = append(limits, scopeCandidateLimits[scope])
		}
		params := cursor.candidateParams(queriedScopes, limits)
		candidates, err := discovery.ListCandidates(ctx, params)
		if err != nil {
			problems = append(problems, fmt.Errorf("list run placement candidates: %w", err))
			return batch, errors.Join(problems...)
		}
		for _, scope := range queriedScopes {
			queried[scope] = struct{}{}
		}
		for _, candidate := range candidates {
			if candidate.ScopeOrdinal < 1 || candidate.ScopeOrdinal > int64(len(queriedScopes)) {
				problems = append(problems, fmt.Errorf(
					"run placement candidate scope ordinal out of range: %d",
					candidate.ScopeOrdinal,
				))
				return batch, errors.Join(problems...)
			}
			scope := queriedScopes[candidate.ScopeOrdinal-1]
			index := scopeIndexes[scope]
			candidate.ScopeOrdinal = int64(index + 1)
			rowsByScope[index] = append(rowsByScope[index], candidate)
		}
	}
	for index, scopeRows := range rowsByScope {
		if _, ok := queried[scopes[index]]; ok && len(scopeRows) == 0 {
			cursor.resetCandidate(scopes[index])
		}
	}
	orgIndex := make(map[pgtype.UUID]int, len(organizations))
	candidatesByOrganization := make([]runPlacementOrganizationCandidates, len(organizations))
	for i, orgID := range organizations {
		orgIndex[orgID] = i
		candidatesByOrganization[i].end = ends[orgID]
	}
	for i, scope := range scopes {
		org, ok := orgIndex[scope.orgID]
		if !ok {
			problems = append(problems, errors.New("run placement scope belongs to an unselected organization"))
			return batch, errors.Join(problems...)
		}
		organization := &candidatesByOrganization[org]
		organization.scopes = append(organization.scopes, runPlacementScopeCandidates{
			scope: scope,
			rows:  rowsByScope[i],
			limit: int(scopeCandidateLimits[scope]),
		})
	}

	blockedScopes := make(map[runPlacementScope]struct{})
	failed := false
	for remaining > 0 {
		var work []runPlacementWork
		for i := range candidatesByOrganization {
			scope, candidate, end, examined, ok := candidatesByOrganization[i].take(blockedScopes)
			if !ok {
				continue
			}
			remaining--
			work = append(work, runPlacementWork{
				scope: scope, candidate: candidate, end: end, examined: examined,
			})
			if remaining <= 0 {
				break
			}
		}
		if len(work) == 0 {
			break
		}
		page, results, err := r.placeRunCandidates(ctx, work)
		batch.add(page)
		for _, result := range results {
			org := &candidatesByOrganization[orgIndex[result.work.scope.orgID]]
			org.examined = max(org.examined, result.work.examined)
			if result.err != nil {
				cursor.deferCandidate(result.work.scope, time.Now().Add(r.runPolicy.pendingInterval))
				blockedScopes[result.work.scope] = struct{}{}
				continue
			}
			if result.outcome == runPlacementPending {
				cursor.deferCandidate(result.work.scope, time.Now().Add(r.runPolicy.pendingInterval))
				blockedScopes[result.work.scope] = struct{}{}
				continue
			}
			cursor.advanceCandidate(result.work.scope, result.work.candidate, result.work.end)
		}
		if err != nil {
			problems = append(problems, err)
			failed = true
			break
		}
	}
	for i, orgID := range organizations {
		organization := &candidatesByOrganization[i]
		if !failed {
			organization.examined = max(organization.examined, organization.seen)
		}
		cursor.advanceScopes(orgID, organization.scopes, organization.examined, organization.end)
	}
	batch.completedOrganizationPass = len(rows) <= int(r.runPolicy.organizationLimit)
	return batch, errors.Join(problems...)
}

func (r *PlacementReconciler) reconcilePendingRunScopes(
	ctx context.Context,
	discovery RunPlacementDiscovery,
	cursor *runPlacementCursor,
	limit int,
) (runPlacementBatch, error) {
	scopes := cursor.duePendingScopes(time.Now(), limit)
	if len(scopes) == 0 {
		return runPlacementBatch{}, nil
	}
	limits := make([]int32, len(scopes))
	for i := range limits {
		limits[i] = 1
	}
	candidates, err := discovery.ListCandidates(ctx, cursor.candidateParams(scopes, limits))
	if err != nil {
		return runPlacementBatch{}, fmt.Errorf("list pending run placement candidates: %w", err)
	}
	work := make([]runPlacementWork, 0, len(candidates))
	seen := make([]bool, len(scopes))
	for _, candidate := range candidates {
		if candidate.ScopeOrdinal < 1 || candidate.ScopeOrdinal > int64(len(scopes)) {
			return runPlacementBatch{}, fmt.Errorf(
				"pending run placement candidate scope ordinal out of range: %d",
				candidate.ScopeOrdinal,
			)
		}
		index := int(candidate.ScopeOrdinal - 1)
		seen[index] = true
		work = append(work, runPlacementWork{scope: scopes[index], candidate: candidate})
	}
	for index, found := range seen {
		if !found {
			cursor.resetCandidate(scopes[index])
		}
	}
	batch, results, err := r.placeRunCandidates(ctx, work)
	for _, result := range results {
		if result.err != nil || result.outcome == runPlacementPending {
			cursor.deferCandidate(result.work.scope, time.Now().Add(r.runPolicy.pendingInterval))
			continue
		}
		cursor.advanceCandidate(result.work.scope, result.work.candidate, false)
	}
	return batch, err
}

func (r *PlacementReconciler) placeRunCandidates(
	ctx context.Context,
	work []runPlacementWork,
) (runPlacementBatch, []runPlacementResult, error) {
	if len(work) == 0 {
		return runPlacementBatch{}, nil, nil
	}
	if r.runParallel == nil {
		var batch runPlacementBatch
		results := make([]runPlacementResult, 0, len(work))
		var problems []error
		for _, item := range work {
			result := r.placeRunCandidate(ctx, item)
			results = append(results, result)
			batch.record(result.outcome, result.err)
			if result.err != nil {
				problems = append(problems, result.err)
			}
		}
		return batch, results, errors.Join(problems...)
	}
	results := make(chan runPlacementResult, len(work))
	var wg sync.WaitGroup
	for _, item := range work {
		select {
		case r.runParallel <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			close(results)
			return collectRunPlacementResults(results, ctx.Err())
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-r.runParallel }()
			results <- r.placeRunCandidate(ctx, item)
		}()
	}
	wg.Wait()
	close(results)
	var batch runPlacementBatch
	completed := make([]runPlacementResult, 0, len(work))
	var problems []error
	for result := range results {
		completed = append(completed, result)
		batch.record(result.outcome, result.err)
		if result.err != nil {
			problems = append(problems, result.err)
		}
	}
	return batch, completed, errors.Join(problems...)
}

func collectRunPlacementResults(
	results <-chan runPlacementResult,
	problem error,
) (runPlacementBatch, []runPlacementResult, error) {
	var batch runPlacementBatch
	var completed []runPlacementResult
	problems := []error{problem}
	for result := range results {
		completed = append(completed, result)
		batch.record(result.outcome, result.err)
		if result.err != nil {
			problems = append(problems, result.err)
		}
	}
	return batch, completed, errors.Join(problems...)
}

func (b *runPlacementBatch) record(outcome runPlacementOutcome, err error) {
	b.attempted++
	if err != nil {
		return
	}
	switch outcome {
	case runPlacementPlaced:
		b.placed++
	case runPlacementPending:
		b.pending++
	case runPlacementChanged:
		b.changed++
	case runPlacementUnavailable:
		b.unavailable++
	}
}

func (r *PlacementReconciler) placeRunCandidate(
	ctx context.Context,
	work runPlacementWork,
) runPlacementResult {
	candidate := ReadyRunCandidate{
		OrgID: work.candidate.OrgID, RunID: work.candidate.RunID,
		ExpectedRunStateVersion: work.candidate.StateVersion,
	}
	placement, err := r.runAuthority.PlaceReadyRun(ctx, candidate)
	if err != nil {
		if errors.Is(err, ErrCandidateChanged) || errors.Is(err, pgx.ErrNoRows) {
			return runPlacementResult{work: work, outcome: runPlacementChanged}
		}
		if errors.Is(err, ErrCapacityUnavailable) {
			return runPlacementResult{work: work, outcome: runPlacementUnavailable}
		}
		return runPlacementResult{work: work, outcome: runPlacementChanged, err: err}
	}
	if !placement.LeaseCreated {
		return runPlacementResult{work: work, outcome: runPlacementPending}
	}
	return runPlacementResult{work: work, outcome: runPlacementPlaced}
}

func (r *PlacementReconciler) ReconcileBuilds(ctx context.Context) error {
	var problems []error
	remaining := r.buildPolicy.limit
	regions, err := r.buildDiscovery.ListQueuedDeploymentBuildRegions(ctx, remaining)
	if err != nil {
		return fmt.Errorf("discover build regions: %w", err)
	}
	for _, region := range regions {
		if remaining <= 0 {
			break
		}
		rows, err := r.buildDiscovery.ListQueuedDeploymentBuildCandidates(ctx, db.ListQueuedDeploymentBuildCandidatesParams{BuildRegionID: region, LimitCount: remaining})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for _, candidate := range rows {
			remaining--
			if err := r.placeBuildCandidate(ctx, ReadyBuildCandidate{
				OrgID: candidate.OrgID, DeploymentID: candidate.DeploymentID,
				BuildRegionID: candidate.BuildRegionID,
				LeaseSequence: candidate.LeaseSequence,
			}); err != nil {
				problems = append(problems, err)
			}
		}
	}
	return errors.Join(problems...)
}

func (r *PlacementReconciler) placeBuildCandidate(ctx context.Context, candidate ReadyBuildCandidate) error {
	_, err := r.buildAuthority.PlaceReadyBuild(ctx, candidate)
	if err != nil {
		if errors.Is(err, ErrCandidateChanged) || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrCapacityUnavailable) {
			return nil
		}
		return err
	}
	return nil
}
