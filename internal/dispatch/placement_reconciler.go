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
	PlaceReadyRun(context.Context, ReadyRunCandidate, pgtype.Timestamptz) (ReadyRunPlacement, error)
}

type BuildPlacementAuthority interface {
	PlaceReadyBuild(context.Context, ReadyBuildCandidate, pgtype.Timestamptz) (db.LeaseQueuedDeploymentBuildRow, error)
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

// Valkey is notified only after commit; a lost notification is repaired by the
// next DB replay.
type PlacementReconciler struct {
	runDiscovery   RunPlacementDiscovery
	runAuthority   RunPlacementAuthority
	buildDiscovery BuildPlacementDiscovery
	buildAuthority BuildPlacementAuthority
	ready          Queue
	wakes          WorkerWakePublisher
	runPolicy      placementLoopPolicy
	buildPolicy    placementLoopPolicy
	metrics        reconcileMetrics
	log            *slog.Logger
}

type placementLoopPolicy struct {
	interval       time.Duration
	failureBackoff time.Duration
	timeout        time.Duration
	limit          int32
}

func NewPlacementReconciler(runDiscovery RunPlacementDiscovery, runAuthority RunPlacementAuthority,
	buildDiscovery BuildPlacementDiscovery, buildAuthority BuildPlacementAuthority,
	ready Queue, wakes WorkerWakePublisher, log *slog.Logger,
) (*PlacementReconciler, error) {
	if runDiscovery == nil || runAuthority == nil || buildDiscovery == nil || buildAuthority == nil || ready == nil || wakes == nil {
		return nil, errors.New("run and build placement discovery, authority, ready index, and wake publisher are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &PlacementReconciler{
		runDiscovery: runDiscovery, runAuthority: runAuthority,
		buildDiscovery: buildDiscovery, buildAuthority: buildAuthority,
		ready: ready, wakes: wakes, log: log, metrics: newReconcileMetrics(),
		runPolicy: placementLoopPolicy{
			interval: defaultRunPlacementInterval, failureBackoff: defaultRunPlacementFailureBackoff,
			timeout: defaultRunPlacementTimeout, limit: defaultRunPlacementLimit,
		},
		buildPolicy: placementLoopPolicy{
			interval: defaultBuildPlacementInterval, failureBackoff: defaultBuildPlacementFailureBackoff,
			timeout: defaultBuildPlacementTimeout, limit: defaultBuildPlacementLimit,
		},
	}, nil
}

func (r *PlacementReconciler) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errC := make(chan error, 2)
	go func() { errC <- r.runLoop(runCtx, "run", r.runPolicy, r.ReconcileRuns) }()
	go func() { errC <- r.runLoop(runCtx, "build", r.buildPolicy, r.ReconcileBuilds) }()
	var firstErr error
	for i := range 2 {
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
	freshAfter := pgtype.Timestamptz{Time: time.Now().UTC().Add(-2 * time.Minute), Valid: true}
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
					ExpectedRunStateVersion: message.RunStateVersion}, freshAfter, message.RunID); err != nil {
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
			QueueClass: scope.QueueClass, QueueName: scope.QueueName})
	}
	fairScopes = (RoundRobinQueueScopeSelector{}).Order(fairScopes)
	for _, scope := range fairScopes {
		if remaining <= 0 {
			break
		}
		rows, err := r.runDiscovery.ListQueuedRunDispatchCandidatesForScope(ctx, db.ListQueuedRunDispatchCandidatesForScopeParams{
			OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			RegionID: scope.RegionID, QueueClass: scope.QueueClass, QueueName: scope.QueueName,
			RowLimit: remaining,
		})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for _, candidate := range rows {
			runID := pgvalue.UUIDString(candidate.RunID)
			if _, duplicate := attempted[runID]; duplicate {
				continue
			}
			remaining--
			if err := r.placeRunCandidate(ctx, ReadyRunCandidate{
				OrgID: candidate.OrgID, RunID: candidate.RunID,
				ExpectedRunStateVersion: candidate.StateVersion,
			}, freshAfter, runID); err != nil {
				problems = append(problems, err)
			}
		}
	}
	return errors.Join(problems...)
}

func (r *PlacementReconciler) placeRunCandidate(ctx context.Context, candidate ReadyRunCandidate, freshAfter pgtype.Timestamptz, runID string) error {
	placement, err := r.runAuthority.PlaceReadyRun(ctx, candidate, freshAfter)
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
		if placement.RuntimeCreated {
			if err := r.wakes.PublishWorkerWake(ctx, WorkerWake{Domain: "runtime",
				WorkerID: placement.WorkerInstanceID, WorkerEpoch: placement.WorkerEpoch,
				RuntimeID: placement.RuntimeInstanceID, AuthorityID: placement.RuntimeInstanceID}); err != nil {
				return fmt.Errorf("publish run runtime wake: %w", err)
			}
			return nil
		}
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
	freshAfter := pgtype.Timestamptz{Time: time.Now().UTC().Add(-2 * time.Minute), Valid: true}
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
					BuildRegionID: message.RegionID, BuildAttemptNumber: message.BuildAttemptNumber, LeaseSequence: message.LeaseSequence,
					RequestedCPUMillis: message.BuildResources.CPUMillis, RequestedMemoryBytes: message.BuildResources.MemoryBytes,
					RequestedWorkloadDiskBytes: message.BuildResources.WorkloadDiskBytes, RequestedScratchBytes: message.BuildResources.ScratchBytes,
					RequestedBuildCacheBytes: message.BuildResources.BuildCacheBytes, RequestedArtifactCacheBytes: message.BuildResources.ArtifactCacheBytes,
					RequestedBuildExecutors: message.BuildResources.Executors}, freshAfter, message.DeploymentID); err != nil {
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
				BuildRegionID: candidate.BuildRegionID, BuildAttemptNumber: candidate.BuildAttemptNumber, LeaseSequence: candidate.LeaseSequence,
				RequestedCPUMillis:          candidate.BuildRequestedCpuMillis,
				RequestedMemoryBytes:        candidate.BuildRequestedMemoryBytes,
				RequestedWorkloadDiskBytes:  candidate.BuildRequestedWorkloadDiskBytes,
				RequestedScratchBytes:       candidate.BuildRequestedScratchBytes,
				RequestedBuildCacheBytes:    candidate.BuildRequestedBuildCacheBytes,
				RequestedArtifactCacheBytes: candidate.BuildRequestedArtifactCacheBytes,
				RequestedBuildExecutors:     candidate.BuildRequestedExecutors,
			}, freshAfter, deploymentID); err != nil {
				problems = append(problems, err)
			}
		}
	}
	return errors.Join(problems...)
}

func (r *PlacementReconciler) placeBuildCandidate(ctx context.Context, candidate ReadyBuildCandidate, freshAfter pgtype.Timestamptz, deploymentID string) error {
	lease, err := r.buildAuthority.PlaceReadyBuild(ctx, candidate, freshAfter)
	fence := fmt.Sprintf("build:%d:%d", candidate.BuildAttemptNumber, candidate.LeaseSequence)
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
