package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultRunPlacementInterval         = time.Second
	defaultRunPlacementFailureBackoff   = time.Second
	defaultRunPlacementTimeout          = 15 * time.Second
	defaultRunPlacementLimit            = int32(32)
	defaultBuildPlacementInterval       = 2 * time.Second
	defaultBuildPlacementFailureBackoff = 5 * time.Second
	defaultBuildPlacementTimeout        = 30 * time.Second
	defaultBuildPlacementLimit          = int32(8)
	defaultWorkspaceExecPlacementLimit  = int32(32)
	defaultWorkspaceExecPendingTimeout  = 10 * time.Minute
)

type RunPlacementDiscovery interface {
	ListQueuedRunCandidateScopes(context.Context, db.ListQueuedRunCandidateScopesParams) ([]db.ListQueuedRunCandidateScopesRow, error)
	ListQueuedRunDispatchCandidatesForScope(context.Context, db.ListQueuedRunDispatchCandidatesForScopeParams) ([]db.ListQueuedRunDispatchCandidatesForScopeRow, error)
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

type WorkerWake struct {
	Domain         string
	WorkerID       pgtype.UUID
	WorkerEpoch    int64
	RuntimeID      pgtype.UUID
	AuthorityID    pgtype.UUID
	RequestVersion int64
}

type WorkerWakePublisher interface {
	PublishWorkerWake(context.Context, WorkerWake) error
}

// PlacementReconciler notifies Valkey only after commit; the next DB replay
// repairs a lost notification.
type PlacementReconciler struct {
	runDiscovery           RunPlacementDiscovery
	runAuthority           RunPlacementAuthority
	buildDiscovery         BuildPlacementDiscovery
	buildAuthority         BuildPlacementAuthority
	workspaceExecDiscovery WorkspaceExecPlacementDiscovery
	workspaceExecAuthority WorkspaceExecPlacementAuthority
	ready                  Queue
	wakes                  WorkerWakePublisher
	runPolicy              placementLoopPolicy
	buildPolicy            placementLoopPolicy
	workspaceExecPolicy    placementLoopPolicy
	metrics                reconcileMetrics
	log                    *slog.Logger
}

type placementLoopPolicy struct {
	interval       time.Duration
	failureBackoff time.Duration
	timeout        time.Duration
	limit          int32
}

func NewPlacementReconciler(runDiscovery RunPlacementDiscovery, runAuthority RunPlacementAuthority,
	buildDiscovery BuildPlacementDiscovery, buildAuthority BuildPlacementAuthority,
	workspaceExecDiscovery WorkspaceExecPlacementDiscovery,
	workspaceExecAuthority WorkspaceExecPlacementAuthority,
	ready Queue, wakes WorkerWakePublisher, log *slog.Logger,
) (*PlacementReconciler, error) {
	if runDiscovery == nil || runAuthority == nil || buildDiscovery == nil || buildAuthority == nil ||
		workspaceExecDiscovery == nil || workspaceExecAuthority == nil || ready == nil || wakes == nil {
		return nil, errors.New("run, build, and workspace exec placement dependencies are required")
	}
	if log == nil {
		log = slog.Default()
	}
	reconciler := &PlacementReconciler{
		runDiscovery: runDiscovery, runAuthority: runAuthority,
		buildDiscovery: buildDiscovery, buildAuthority: buildAuthority,
		workspaceExecDiscovery: workspaceExecDiscovery,
		workspaceExecAuthority: workspaceExecAuthority,
		ready:                  ready, wakes: wakes, log: log, metrics: newReconcileMetrics(),
		runPolicy: placementLoopPolicy{
			interval: defaultRunPlacementInterval, failureBackoff: defaultRunPlacementFailureBackoff,
			timeout: defaultRunPlacementTimeout, limit: defaultRunPlacementLimit,
		},
		buildPolicy: placementLoopPolicy{
			interval: defaultBuildPlacementInterval, failureBackoff: defaultBuildPlacementFailureBackoff,
			timeout: defaultBuildPlacementTimeout, limit: defaultBuildPlacementLimit,
		},
		workspaceExecPolicy: placementLoopPolicy{
			interval: defaultRunPlacementInterval, failureBackoff: defaultRunPlacementFailureBackoff,
			timeout: defaultRunPlacementTimeout, limit: defaultWorkspaceExecPlacementLimit,
		},
	}
	return reconciler, nil
}

func (r *PlacementReconciler) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	loops := 3
	errC := make(chan error, loops)
	go func() { errC <- r.runLoop(runCtx, "run", r.runPolicy, r.ReconcileRuns) }()
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
		placement, err := r.workspaceExecAuthority.PlaceWorkspaceExec(
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
		if !placement.ProcessBound && placement.WorkspaceMountID.Valid {
			if err := r.wakes.PublishWorkerWake(ctx, WorkerWake{
				Domain:      "workspace",
				WorkerID:    placement.WorkerInstanceID,
				WorkerEpoch: placement.WorkerEpoch,
				RuntimeID:   placement.RuntimeInstanceID,
				AuthorityID: placement.WorkspaceMountID,
			}); err != nil {
				problems = append(problems, fmt.Errorf("publish workspace exec mount wake: %w", err))
			}
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
	remaining := r.runPolicy.limit
	attempted := make(map[string]struct{}, r.runPolicy.limit)
	var problems []error

	// Ready-index entries are hints; Postgres revalidates every authority fence.
	regions, indexErr := r.ready.ReadyRegions(ctx, WorkKindRun, int64(r.runPolicy.limit))
	if indexErr != nil {
		r.log.Warn("ready index unavailable; using bounded Postgres fallback", "error", indexErr)
	} else {
		for _, region := range regions {
			if remaining <= 0 {
				break
			}
			messages, err := r.ready.SelectReady(ctx, ReadySelection{WorkKind: WorkKindRun, RegionID: region, Limit: int(remaining)})
			if err != nil {
				r.log.Warn("ready index selection failed; using bounded Postgres fallback", "region_id", region, "error", err)
				continue
			}
			for _, message := range messages {
				orgID, err := parseUUID(message.OrgID)
				if err != nil {
					_ = r.ready.RemoveReady(ctx, WorkKindRun, message.RunID, message.ReadyFence())
					continue
				}
				runID, err := parseUUID(message.RunID)
				if err != nil {
					_ = r.ready.RemoveReady(ctx, WorkKindRun, message.RunID, message.ReadyFence())
					continue
				}
				attempted[message.RunID] = struct{}{}
				remaining--
				if err := r.placeRunCandidate(ctx, ReadyRunCandidate{OrgID: orgID, RunID: runID,
					ExpectedRunStateVersion: message.RunStateVersion}, message.RunID); err != nil {
					problems = append(problems, err)
				}
			}
		}
	}

	if remaining <= 0 {
		return errors.Join(problems...)
	}
	scopes, err := r.runDiscovery.ListQueuedRunCandidateScopes(ctx, db.ListQueuedRunCandidateScopesParams{
		RowLimit: remaining, ScanSeed: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("list ready run scopes: %w", err)
	}
	fairScopes := make([]QueueScope, 0, len(scopes))
	for _, scope := range scopes {
		fairScopes = append(fairScopes, QueueScope{OrgID: scope.OrgID, ProjectID: scope.ProjectID,
			EnvironmentID: scope.EnvironmentID, RegionID: scope.RegionID,
			ConcurrencyKey: scope.ConcurrencyKey, QueueName: scope.QueueName})
	}
	fairScopes = (RoundRobinQueueScopeSelector{}).Order(fairScopes)
	type scopeCandidates struct {
		rows []db.ListQueuedRunDispatchCandidatesForScopeRow
	}
	candidatesByScope := make([]scopeCandidates, 0, len(fairScopes))
	for _, scope := range fairScopes {
		rows, err := r.runDiscovery.ListQueuedRunDispatchCandidatesForScope(ctx, db.ListQueuedRunDispatchCandidatesForScopeParams{
			OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			RegionID: scope.RegionID,
			ConcurrencyKey: pgtype.Text{
				String: scope.ConcurrencyKey,
				Valid:  scope.ConcurrencyKey != "",
			},
			QueueName: scope.QueueName,
			RowLimit:  remaining,
		})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if len(rows) > 0 {
			candidatesByScope = append(candidatesByScope, scopeCandidates{rows: rows})
		}
	}
	for index := 0; remaining > 0; index++ {
		madeProgress := false
		for _, scope := range candidatesByScope {
			if index >= len(scope.rows) {
				continue
			}
			madeProgress = true
			candidate := scope.rows[index]
			runID := pgvalue.UUIDString(candidate.RunID)
			if _, duplicate := attempted[runID]; duplicate {
				continue
			}
			remaining--
			if err := r.placeRunCandidate(ctx, ReadyRunCandidate{
				OrgID: candidate.OrgID, RunID: candidate.RunID,
				ExpectedRunStateVersion: candidate.StateVersion,
			}, runID); err != nil {
				problems = append(problems, err)
			}
			if remaining <= 0 {
				break
			}
		}
		if !madeProgress {
			break
		}
	}
	return errors.Join(problems...)
}

func (r *PlacementReconciler) placeRunCandidate(ctx context.Context, candidate ReadyRunCandidate, runID string) error {
	placement, err := r.runAuthority.PlaceReadyRun(ctx, candidate)
	if err != nil {
		if errors.Is(err, ErrCandidateChanged) {
			if cleanupErr := r.ready.RemoveReady(ctx, WorkKindRun, runID, fmt.Sprintf("run:%d", candidate.ExpectedRunStateVersion)); cleanupErr != nil {
				return fmt.Errorf("remove stale ready candidate: %w", cleanupErr)
			}
			return nil
		}
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrCapacityUnavailable) {
			return nil
		}
		return err
	}
	if !placement.LeaseCreated {
		if placement.WorkspaceMountID.Valid {
			if err := r.wakes.PublishWorkerWake(ctx, WorkerWake{Domain: "workspace",
				WorkerID: placement.WorkerInstanceID, WorkerEpoch: placement.WorkerEpoch,
				RuntimeID: placement.RuntimeInstanceID, AuthorityID: placement.WorkspaceMountID}); err != nil {
				return fmt.Errorf("publish run workspace wake: %w", err)
			}
		}
		return nil
	}
	if err := r.ready.RemoveReady(ctx, WorkKindRun, runID, fmt.Sprintf("run:%d", candidate.ExpectedRunStateVersion)); err != nil {
		r.log.Warn("placed run remains in reconstructable ready index", "run_id", runID, "error", err)
	}
	lease := placement.Lease
	if err := r.wakes.PublishWorkerWake(ctx, WorkerWake{Domain: "run", WorkerID: lease.WorkerInstanceID,
		WorkerEpoch: lease.WorkerEpoch, RuntimeID: lease.RuntimeInstanceID, AuthorityID: lease.ID}); err != nil {
		return fmt.Errorf("publish run wake: %w", err)
	}
	return nil
}

func parseUUID(value string) (pgtype.UUID, error) {
	var parsed pgtype.UUID
	if err := parsed.Scan(value); err != nil {
		return pgtype.UUID{}, err
	}
	return parsed, nil
}

func (r *PlacementReconciler) ReconcileBuilds(ctx context.Context) error {
	var problems []error
	remaining := r.buildPolicy.limit
	attempted := make(map[string]struct{}, r.buildPolicy.limit)
	regions, indexErr := r.ready.ReadyRegions(ctx, WorkKindBuild, int64(r.buildPolicy.limit))
	if indexErr == nil {
		for _, region := range regions {
			if remaining <= 0 {
				break
			}
			messages, err := r.ready.SelectReady(ctx, ReadySelection{WorkKind: WorkKindBuild, RegionID: region, Limit: int(remaining)})
			if err != nil {
				r.log.Warn("build ready index selection failed; using Postgres fallback", "error", err)
				continue
			}
			for _, message := range messages {
				orgID, orgErr := parseUUID(message.OrgID)
				deploymentID, deploymentErr := parseUUID(message.DeploymentID)
				if orgErr != nil || deploymentErr != nil {
					_ = r.ready.RemoveReady(ctx, WorkKindBuild, message.DeploymentID, message.ReadyFence())
					continue
				}
				attempted[message.DeploymentID] = struct{}{}
				remaining--
				if err := r.placeBuildCandidate(ctx, ReadyBuildCandidate{OrgID: orgID, DeploymentID: deploymentID,
					BuildRegionID: message.RegionID, LeaseSequence: message.LeaseSequence}, message.DeploymentID); err != nil {
					problems = append(problems, err)
				}
			}
		}
	} else {
		r.log.Warn("build ready index unavailable; using Postgres fallback", "error", indexErr)
	}
	if remaining <= 0 {
		return errors.Join(problems...)
	}
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
			deploymentID := pgvalue.UUIDString(candidate.DeploymentID)
			if _, duplicate := attempted[deploymentID]; duplicate {
				continue
			}
			remaining--
			if err := r.placeBuildCandidate(ctx, ReadyBuildCandidate{
				OrgID: candidate.OrgID, DeploymentID: candidate.DeploymentID,
				BuildRegionID: candidate.BuildRegionID,
				LeaseSequence: candidate.LeaseSequence,
			}, deploymentID); err != nil {
				problems = append(problems, err)
			}
		}
	}
	return errors.Join(problems...)
}

func (r *PlacementReconciler) placeBuildCandidate(ctx context.Context, candidate ReadyBuildCandidate, deploymentID string) error {
	lease, err := r.buildAuthority.PlaceReadyBuild(ctx, candidate)
	fence := fmt.Sprintf("build:%d", candidate.LeaseSequence)
	if err != nil {
		if errors.Is(err, ErrCandidateChanged) {
			return r.ready.RemoveReady(ctx, WorkKindBuild, deploymentID, fence)
		}
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrCapacityUnavailable) {
			return nil
		}
		return err
	}
	if err := r.ready.RemoveReady(ctx, WorkKindBuild, deploymentID, fence); err != nil {
		r.log.Warn("placed build remains in reconstructable ready index", "deployment_id", deploymentID, "error", err)
	}
	if err := r.wakes.PublishWorkerWake(ctx, WorkerWake{Domain: "build", WorkerID: lease.WorkerInstanceID,
		WorkerEpoch: lease.WorkerEpoch, AuthorityID: lease.ID}); err != nil {
		return fmt.Errorf("publish build wake: %w", err)
	}
	return nil
}
