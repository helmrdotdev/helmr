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
	defaultRunDispatchIdleInterval       = time.Second
	defaultRunDispatchFailureBackoff     = time.Second
	defaultRunDispatchTimeout            = 15 * time.Second
	defaultRunDispatchWorkers            = 8
	defaultRunDispatchOrganizationLimit  = int32(32)
	defaultRunDispatchAttemptLimit       = int32(32)
	defaultRunDispatchParallelism        = 16
	defaultRunDispatchPendingInterval    = time.Second
	defaultCommandDispatchLimit          = int32(32)
	defaultComputerCommandPendingTimeout = 10 * time.Minute
)

type RunDiscovery interface {
	ListOrganizations(context.Context, int16, pgtype.UUID, int32) ([]pgtype.UUID, error)
	ListScopes(context.Context, runDispatchScopeParams) ([]runDispatchScopeRow, error)
	ListCandidates(context.Context, db.ListQueuedRunDispatchCandidatesParams) ([]db.ListQueuedRunDispatchCandidatesRow, error)
}

type RunAssigner interface {
	AssignRun(context.Context, RunCandidate) (RunAssignment, error)
}

type CommandDiscovery interface {
	ListPendingComputerCommandCandidates(
		context.Context,
		int32,
	) ([]db.ListPendingComputerCommandCandidatesRow, error)
	ListRecoverableComputerCommandCandidates(
		context.Context,
		int32,
	) ([]db.ListRecoverableComputerCommandCandidatesRow, error)
}

type CommandAuthority interface {
	AssignCommand(
		context.Context,
		CommandCandidate,
	) (CommandAssignment, error)
	RecoverComputerCommand(
		context.Context,
		RecoverableComputerCommandCandidate,
	) error
	FailPendingComputerCommand(
		context.Context,
		CommandCandidate,
		string,
	) error
}

type Reconciler struct {
	runDiscovery             RunDiscovery
	runLaneLocker            RunLaneLocker
	runAuthority             RunAssigner
	computerCommandDiscovery CommandDiscovery
	computerCommandAuthority CommandAuthority
	runPolicy                runDispatchPolicy
	computerCommandPolicy    loopPolicy
	runCursors               [runLaneCount]runDispatchCursor
	runLaneMutexes           [runLaneCount]sync.Mutex
	runNextLane              atomic.Uint32
	runParallel              chan struct{}
	metrics                  reconcileMetrics
	log                      *slog.Logger
}

type loopPolicy struct {
	interval       time.Duration
	failureBackoff time.Duration
	timeout        time.Duration
	limit          int32
}

type runDispatchPolicy struct {
	idleInterval      time.Duration
	failureBackoff    time.Duration
	timeout           time.Duration
	workers           int
	organizationLimit int32
	attemptLimit      int32
	parallelism       int
	pendingInterval   time.Duration
}

type runDispatchOutcome uint8

const (
	runDispatchAssigned runDispatchOutcome = iota
	runDispatchPending
	runDispatchChanged
	runDispatchUnavailable
)

type runDispatchBatch struct {
	attempted                 int
	assigned                  int
	pending                   int
	changed                   int
	unavailable               int
	completedOrganizationPass bool
}

type runDispatchResult struct {
	work    runDispatchWork
	outcome runDispatchOutcome
	err     error
}

type runDispatchWork struct {
	scope     runDispatchScope
	candidate db.ListQueuedRunDispatchCandidatesRow
	end       bool
	examined  int
}

func (b runDispatchBatch) capacityBlocked() bool {
	return b.attempted > 0 && b.unavailable == b.attempted
}

func (b *runDispatchBatch) add(page runDispatchBatch) {
	b.attempted += page.attempted
	b.assigned += page.assigned
	b.pending += page.pending
	b.changed += page.changed
	b.unavailable += page.unavailable
	b.completedOrganizationPass = b.completedOrganizationPass || page.completedOrganizationPass
}

type runDispatchScopeCandidates struct {
	scope runDispatchScope
	rows  []db.ListQueuedRunDispatchCandidatesRow
	next  int
	limit int
}

type runDispatchOrganizationCandidates struct {
	scopes   []runDispatchScopeCandidates
	next     int
	seen     int
	examined int
	end      bool
}

func (c *runDispatchOrganizationCandidates) take(
	blocked map[runDispatchScope]struct{},
) (runDispatchScope, db.ListQueuedRunDispatchCandidatesRow, bool, int, bool) {
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
	return runDispatchScope{}, db.ListQueuedRunDispatchCandidatesRow{}, false, c.seen, false
}

func NewReconciler(runDiscovery RunDiscovery, runLaneLocker RunLaneLocker,
	runAuthority RunAssigner,
	computerCommandDiscovery CommandDiscovery,
	computerCommandAuthority CommandAuthority,
	log *slog.Logger,
) (*Reconciler, error) {
	if runDiscovery == nil || runLaneLocker == nil || runAuthority == nil ||
		computerCommandDiscovery == nil || computerCommandAuthority == nil {
		return nil, errors.New("run and command dispatch dependencies are required")
	}
	if log == nil {
		log = slog.Default()
	}
	reconciler := &Reconciler{
		runDiscovery: runDiscovery, runLaneLocker: runLaneLocker, runAuthority: runAuthority,
		computerCommandDiscovery: computerCommandDiscovery,
		computerCommandAuthority: computerCommandAuthority,
		log:                      log, metrics: newReconcileMetrics(),
		runPolicy: runDispatchPolicy{
			idleInterval:      defaultRunDispatchIdleInterval,
			failureBackoff:    defaultRunDispatchFailureBackoff,
			timeout:           defaultRunDispatchTimeout,
			workers:           defaultRunDispatchWorkers,
			organizationLimit: defaultRunDispatchOrganizationLimit,
			attemptLimit:      defaultRunDispatchAttemptLimit,
			parallelism:       defaultRunDispatchParallelism,
			pendingInterval:   defaultRunDispatchPendingInterval,
		},
		computerCommandPolicy: loopPolicy{
			interval: defaultRunDispatchIdleInterval, failureBackoff: defaultRunDispatchFailureBackoff,
			timeout: defaultRunDispatchTimeout, limit: defaultCommandDispatchLimit,
		},
	}
	reconciler.runParallel = make(chan struct{}, reconciler.runPolicy.parallelism)
	return reconciler, nil
}

func (r *Reconciler) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	loops := r.runPolicy.workers + 1
	errC := make(chan error, loops)
	for range r.runPolicy.workers {
		go func() { errC <- r.runLaneLoop(runCtx) }()
	}
	go func() {
		errC <- r.runLoop(
			runCtx,
			"computer_command",
			r.computerCommandPolicy,
			r.ReconcileComputerCommands,
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

func (r *Reconciler) runLaneLoop(ctx context.Context) error {
	idleLanes := 0
	for {
		lane := int16((r.runNextLane.Add(1) - 1) % uint32(runLaneCount))
		started := time.Now()
		cycleCtx, cancel := context.WithTimeout(ctx, r.runPolicy.timeout)
		guard, locked, err := r.runLaneLocker.TryLock(cycleCtx, lane)
		if err != nil {
			cancel()
			r.metrics.observe(ctx, "dispatch", "run", "failure", time.Since(started))
			r.log.Warn("run dispatch lane failed", "duration_ms", time.Since(started).Milliseconds(), "error", err)
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
			// unassignable work. The guard and delay are bounded and carry no durable
			// scheduling state.
			reconcileErr = waitFor(ctx, r.runPolicy.idleInterval)
		}
		unlockErr := guard.Unlock()
		cancel()
		err = errors.Join(reconcileErr, unlockErr)
		outcome := "success"
		if err != nil {
			outcome = "failure"
			r.log.Warn("run dispatch reconciliation failed", "duration_ms", time.Since(started).Milliseconds(), "error", err)
		}
		r.metrics.observe(ctx, "dispatch", "run", outcome, time.Since(started))
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

func (r *Reconciler) waitAfterRunLane(ctx context.Context, idleLanes *int) error {
	workers := max(1, r.runPolicy.workers)
	idleLimit := (runLaneCount + workers - 1) / workers
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

func (r *Reconciler) ReconcileComputerCommands(ctx context.Context) error {
	recoverable, err := r.computerCommandDiscovery.ListRecoverableComputerCommandCandidates(
		ctx,
		r.computerCommandPolicy.limit,
	)
	if err != nil {
		return fmt.Errorf("list recoverable computer commands: %w", err)
	}
	var problems []error
	for _, row := range recoverable {
		err := r.computerCommandAuthority.RecoverComputerCommand(
			ctx,
			RecoverableComputerCommandCandidate{
				OrgID:            row.OrgID,
				CommandID:        row.ID,
				ComputerID:       row.ComputerID,
				ExpectedRevision: row.Revision,
			},
		)
		if errors.Is(err, ErrCandidateChanged) || errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			problems = append(problems, err)
		}
	}
	rows, err := r.computerCommandDiscovery.ListPendingComputerCommandCandidates(
		ctx,
		r.computerCommandPolicy.limit,
	)
	if err != nil {
		return fmt.Errorf("list pending computer commands: %w", err)
	}
	expiredBefore := time.Now().UTC().Add(-defaultComputerCommandPendingTimeout)
	for _, row := range rows {
		candidate := CommandCandidate{
			OrgID:            row.OrgID,
			CommandID:        row.ID,
			ExpectedRevision: row.Revision,
		}
		if !row.CreatedAt.Time.After(expiredBefore) {
			err := r.computerCommandAuthority.FailPendingComputerCommand(
				ctx,
				candidate,
				"computer_command_placement_timed_out",
			)
			if errors.Is(err, ErrCandidateChanged) || errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				problems = append(problems, err)
			}
			continue
		}
		_, err := r.computerCommandAuthority.AssignCommand(
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

func (r *Reconciler) runLoop(ctx context.Context, domain string, policy loopPolicy, reconcile func(context.Context) error) error {
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
			r.log.Warn("reconciliation failed", "domain", domain, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		}
		r.metrics.observe(ctx, "dispatch", domain, outcome, time.Since(started))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Reconciler) ReconcileRuns(ctx context.Context) error {
	_, err := r.reconcileRunLane(ctx, 0, r.runDiscovery)
	return err
}

func (r *Reconciler) reconcileRunLane(
	ctx context.Context,
	lane int16,
	discovery RunDiscovery,
) (runDispatchBatch, error) {
	if lane < 0 || lane >= runLaneCount {
		return runDispatchBatch{}, errors.New("run dispatch lane is out of range")
	}
	if discovery == nil {
		return runDispatchBatch{}, errors.New("run dispatch discovery is required")
	}
	r.runLaneMutexes[lane].Lock()
	defer r.runLaneMutexes[lane].Unlock()
	r.runCursors[lane].beginCycle()
	remaining := r.runPolicy.attemptLimit
	var batch runDispatchBatch
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

func (r *Reconciler) reconcileRunLanePage(
	ctx context.Context,
	lane int16,
	discovery RunDiscovery,
	attemptLimit int32,
) (runDispatchBatch, error) {
	cursor := &r.runCursors[lane]
	remaining := attemptLimit
	var batch runDispatchBatch
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
		problems = append(problems, fmt.Errorf("list run dispatch organizations: %w", err))
		return batch, errors.Join(problems...)
	}
	organizations := cursor.chooseOrganizations(rows, int(r.runPolicy.organizationLimit))
	if len(organizations) == 0 {
		return batch, errors.Join(problems...)
	}
	scopeShare := (int(remaining) + len(organizations) - 1) / len(organizations)
	scopeFetchLimit := int32(min(scopeShare, runDispatchCandidateScopeLimit) + 1)
	scopeRows, err := discovery.ListScopes(
		ctx,
		cursor.scopeParams(organizations, scopeFetchLimit),
	)
	if err != nil {
		problems = append(problems, fmt.Errorf("list run dispatch scopes: %w", err))
		return batch, errors.Join(problems...)
	}
	scopeLimit := min(int(remaining), runDispatchCandidateScopeLimit)
	scopes, ends := cursor.chooseScopes(scopeRows, organizations, scopeLimit, int(scopeFetchLimit))
	if len(scopes) == 0 {
		batch.completedOrganizationPass = len(rows) <= int(r.runPolicy.organizationLimit)
		return batch, errors.Join(problems...)
	}
	rowsByScope := make([][]db.ListQueuedRunDispatchCandidatesRow, len(scopes))
	queriedScopes := cursor.readyCandidateScopes(scopes)
	scopesPerOrganization := make(map[pgtype.UUID]int, len(organizations))
	for _, scope := range scopes {
		scopesPerOrganization[scope.orgID]++
	}
	scopeCandidateLimits := make(map[runDispatchScope]int32, len(scopes))
	for _, scope := range scopes {
		count := scopesPerOrganization[scope.orgID]
		limit := (scopeShare + count - 1) / count
		scopeCandidateLimits[scope] = int32(min(limit, runDispatchCandidateScopeLimit))
	}
	queried := make(map[runDispatchScope]struct{}, len(queriedScopes))
	scopeIndexes := make(map[runDispatchScope]int, len(scopes))
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
			problems = append(problems, fmt.Errorf("list run dispatch candidates: %w", err))
			return batch, errors.Join(problems...)
		}
		for _, scope := range queriedScopes {
			queried[scope] = struct{}{}
		}
		for _, candidate := range candidates {
			if candidate.ScopeOrdinal < 1 || candidate.ScopeOrdinal > int64(len(queriedScopes)) {
				problems = append(problems, fmt.Errorf(
					"run dispatch candidate scope ordinal out of range: %d",
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
	candidatesByOrganization := make([]runDispatchOrganizationCandidates, len(organizations))
	for i, orgID := range organizations {
		orgIndex[orgID] = i
		candidatesByOrganization[i].end = ends[orgID]
	}
	for i, scope := range scopes {
		org, ok := orgIndex[scope.orgID]
		if !ok {
			problems = append(problems, errors.New("run dispatch scope belongs to an unselected organization"))
			return batch, errors.Join(problems...)
		}
		organization := &candidatesByOrganization[org]
		organization.scopes = append(organization.scopes, runDispatchScopeCandidates{
			scope: scope,
			rows:  rowsByScope[i],
			limit: int(scopeCandidateLimits[scope]),
		})
	}

	blockedScopes := make(map[runDispatchScope]struct{})
	failed := false
	for remaining > 0 {
		var work []runDispatchWork
		for i := range candidatesByOrganization {
			scope, candidate, end, examined, ok := candidatesByOrganization[i].take(blockedScopes)
			if !ok {
				continue
			}
			remaining--
			work = append(work, runDispatchWork{
				scope: scope, candidate: candidate, end: end, examined: examined,
			})
			if remaining <= 0 {
				break
			}
		}
		if len(work) == 0 {
			break
		}
		page, results, err := r.assignRunCandidates(ctx, work)
		batch.add(page)
		for _, result := range results {
			org := &candidatesByOrganization[orgIndex[result.work.scope.orgID]]
			org.examined = max(org.examined, result.work.examined)
			if result.err != nil {
				cursor.deferCandidate(result.work.scope, time.Now().Add(r.runPolicy.pendingInterval))
				blockedScopes[result.work.scope] = struct{}{}
				continue
			}
			if result.outcome == runDispatchPending {
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

func (r *Reconciler) reconcilePendingRunScopes(
	ctx context.Context,
	discovery RunDiscovery,
	cursor *runDispatchCursor,
	limit int,
) (runDispatchBatch, error) {
	scopes := cursor.duePendingScopes(time.Now(), limit)
	if len(scopes) == 0 {
		return runDispatchBatch{}, nil
	}
	limits := make([]int32, len(scopes))
	for i := range limits {
		limits[i] = 1
	}
	candidates, err := discovery.ListCandidates(ctx, cursor.candidateParams(scopes, limits))
	if err != nil {
		return runDispatchBatch{}, fmt.Errorf("list pending run dispatch candidates: %w", err)
	}
	work := make([]runDispatchWork, 0, len(candidates))
	seen := make([]bool, len(scopes))
	for _, candidate := range candidates {
		if candidate.ScopeOrdinal < 1 || candidate.ScopeOrdinal > int64(len(scopes)) {
			return runDispatchBatch{}, fmt.Errorf(
				"pending run dispatch candidate scope ordinal out of range: %d",
				candidate.ScopeOrdinal,
			)
		}
		index := int(candidate.ScopeOrdinal - 1)
		seen[index] = true
		work = append(work, runDispatchWork{scope: scopes[index], candidate: candidate})
	}
	for index, found := range seen {
		if !found {
			cursor.resetCandidate(scopes[index])
		}
	}
	batch, results, err := r.assignRunCandidates(ctx, work)
	for _, result := range results {
		if result.err != nil || result.outcome == runDispatchPending {
			cursor.deferCandidate(result.work.scope, time.Now().Add(r.runPolicy.pendingInterval))
			continue
		}
		cursor.advanceCandidate(result.work.scope, result.work.candidate, false)
	}
	return batch, err
}

func (r *Reconciler) assignRunCandidates(
	ctx context.Context,
	work []runDispatchWork,
) (runDispatchBatch, []runDispatchResult, error) {
	if len(work) == 0 {
		return runDispatchBatch{}, nil, nil
	}
	if r.runParallel == nil {
		var batch runDispatchBatch
		results := make([]runDispatchResult, 0, len(work))
		var problems []error
		for _, item := range work {
			result := r.assignRunCandidate(ctx, item)
			results = append(results, result)
			batch.record(result.outcome, result.err)
			if result.err != nil {
				problems = append(problems, result.err)
			}
		}
		return batch, results, errors.Join(problems...)
	}
	results := make(chan runDispatchResult, len(work))
	var wg sync.WaitGroup
	for _, item := range work {
		select {
		case r.runParallel <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			close(results)
			return collectRunDispatchResults(results, ctx.Err())
		}
		wg.Go(func() {
			defer func() { <-r.runParallel }()
			results <- r.assignRunCandidate(ctx, item)
		})
	}
	wg.Wait()
	close(results)
	var batch runDispatchBatch
	completed := make([]runDispatchResult, 0, len(work))
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

func collectRunDispatchResults(
	results <-chan runDispatchResult,
	problem error,
) (runDispatchBatch, []runDispatchResult, error) {
	var batch runDispatchBatch
	var completed []runDispatchResult
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

func (b *runDispatchBatch) record(outcome runDispatchOutcome, err error) {
	b.attempted++
	if err != nil {
		return
	}
	switch outcome {
	case runDispatchAssigned:
		b.assigned++
	case runDispatchPending:
		b.pending++
	case runDispatchChanged:
		b.changed++
	case runDispatchUnavailable:
		b.unavailable++
	}
}

func (r *Reconciler) assignRunCandidate(
	ctx context.Context,
	work runDispatchWork,
) runDispatchResult {
	candidate := RunCandidate{
		OrgID: work.candidate.OrgID, RunID: work.candidate.RunID,
		ExpectedRunRevision: work.candidate.Revision,
	}
	assignment, err := r.runAuthority.AssignRun(ctx, candidate)
	if err != nil {
		if errors.Is(err, ErrCandidateChanged) || errors.Is(err, pgx.ErrNoRows) {
			return runDispatchResult{work: work, outcome: runDispatchChanged}
		}
		if errors.Is(err, ErrCapacityUnavailable) {
			return runDispatchResult{work: work, outcome: runDispatchUnavailable}
		}
		return runDispatchResult{work: work, outcome: runDispatchChanged, err: err}
	}
	if !assignment.LeaseCreated {
		return runDispatchResult{work: work, outcome: runDispatchPending}
	}
	return runDispatchResult{work: work, outcome: runDispatchAssigned}
}
