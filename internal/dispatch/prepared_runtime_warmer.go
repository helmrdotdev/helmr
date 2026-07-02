package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/runtime"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	DefaultRuntimePrepareInterval                = 5 * time.Second
	DefaultRuntimePrepareLimit                   = int32(20)
	DefaultRuntimePrepareConsecutiveFailureLimit = 3
	preparedRuntimeWarmUnlockTimeout             = 5 * time.Second
	defaultPreparedRuntimeInstanceTTL            = 5 * time.Minute
)

type RuntimePreparerStore interface {
	ReleaseExpiredPreparedRuntimeReservations(context.Context, pgtype.Timestamptz) ([]db.ReleaseExpiredPreparedRuntimeReservationsRow, error)
	CreateSupersededPreparedRuntimeStopCommands(context.Context, int32) ([]db.WorkerCommand, error)
	ListRuntimeInstanceWarmTargets(context.Context, db.ListRuntimeInstanceWarmTargetsParams) ([]db.ListRuntimeInstanceWarmTargetsRow, error)
	ListRuntimeSubstratePrepareTargets(context.Context, db.ListRuntimeSubstratePrepareTargetsParams) ([]db.ListRuntimeSubstratePrepareTargetsRow, error)
	CreateRuntimeInstanceForDeploymentSandbox(context.Context, db.CreateRuntimeInstanceForDeploymentSandboxParams) (db.CreateRuntimeInstanceForDeploymentSandboxRow, error)
	MarkRuntimeInstanceFailed(context.Context, db.MarkRuntimeInstanceFailedParams) (db.RuntimeInstance, error)
	CreateWorkerCommand(context.Context, db.CreateWorkerCommandParams) (db.WorkerCommand, error)
}

type RuntimePrepareLock interface {
	TryLock(ctx context.Context) (RuntimePrepareLockGuard, bool, error)
}

type RuntimePrepareLockGuard interface {
	Store(fallback RuntimePreparerStore) RuntimePreparerStore
	Unlock(ctx context.Context) error
}

type RuntimePreparer struct {
	store        RuntimePreparerStore
	lock         RuntimePrepareLock
	every        time.Duration
	targetCount  int32
	limit        int32
	failureLimit int
	log          *slog.Logger
}

type RuntimePreparerOption func(*RuntimePreparer)

func WithRuntimePrepareInterval(every time.Duration) RuntimePreparerOption {
	return func(warmer *RuntimePreparer) {
		warmer.every = every
	}
}

func WithRuntimePrepareTarget(target int32) RuntimePreparerOption {
	return func(warmer *RuntimePreparer) {
		warmer.targetCount = target
	}
}

func WithRuntimePrepareLimit(limit int32) RuntimePreparerOption {
	return func(warmer *RuntimePreparer) {
		warmer.limit = limit
	}
}

func WithRuntimePrepareConsecutiveFailureLimit(limit int) RuntimePreparerOption {
	return func(warmer *RuntimePreparer) {
		warmer.failureLimit = limit
	}
}

func WithRuntimePrepareLogger(log *slog.Logger) RuntimePreparerOption {
	return func(warmer *RuntimePreparer) {
		warmer.log = log
	}
}

func WithRuntimePrepareLock(lock RuntimePrepareLock) RuntimePreparerOption {
	return func(warmer *RuntimePreparer) {
		warmer.lock = lock
	}
}

func NewRuntimePreparer(store RuntimePreparerStore, opts ...RuntimePreparerOption) (*RuntimePreparer, error) {
	if store == nil {
		return nil, errors.New("prepared runtime warmer store is required")
	}
	warmer := &RuntimePreparer{
		store:        store,
		every:        DefaultRuntimePrepareInterval,
		targetCount:  0,
		limit:        DefaultRuntimePrepareLimit,
		failureLimit: DefaultRuntimePrepareConsecutiveFailureLimit,
		log:          slog.Default(),
	}
	for _, opt := range opts {
		opt(warmer)
	}
	if warmer.every <= 0 {
		return nil, errors.New("prepared runtime warm interval must be positive")
	}
	if warmer.targetCount < 0 {
		return nil, errors.New("prepared runtime warm target must be non-negative")
	}
	if warmer.limit <= 0 {
		return nil, errors.New("prepared runtime warm limit must be positive")
	}
	if warmer.failureLimit <= 0 {
		return nil, errors.New("prepared runtime warm consecutive failure limit must be positive")
	}
	if warmer.log == nil {
		warmer.log = slog.Default()
	}
	return warmer, nil
}

func (w *RuntimePreparer) Run(ctx context.Context) error {
	if w.targetCount == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if err := w.WarmOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			consecutiveFailures++
			w.log.Warn("prepared runtime warm failed", "error", err, "consecutive_failures", consecutiveFailures)
			if consecutiveFailures >= w.failureLimit {
				return fmt.Errorf("prepared runtime warm failed %d consecutive times: %w", consecutiveFailures, err)
			}
		} else {
			consecutiveFailures = 0
		}
		timer.Reset(w.every)
	}
}

func (w *RuntimePreparer) WarmOnce(ctx context.Context) error {
	return w.Reconcile(ctx)
}

func (w *RuntimePreparer) Reconcile(ctx context.Context) error {
	return w.reconcile(ctx, pgtype.UUID{})
}

func (w *RuntimePreparer) ReconcileDeploymentSandbox(ctx context.Context, deploymentSandboxID pgtype.UUID) error {
	if !deploymentSandboxID.Valid {
		return errors.New("deployment sandbox id is required")
	}
	return w.reconcile(ctx, deploymentSandboxID)
}

func (w *RuntimePreparer) reconcile(ctx context.Context, deploymentSandboxID pgtype.UUID) error {
	if w.targetCount == 0 {
		return nil
	}
	var guard RuntimePrepareLockGuard
	store := w.store
	if w.lock != nil {
		var locked bool
		var err error
		guard, locked, err = w.lock.TryLock(ctx)
		if err != nil {
			return err
		}
		if !locked {
			w.log.Debug("prepared runtime warm lock is held by another instance")
			return nil
		}
		store = guard.Store(w.store)
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), preparedRuntimeWarmUnlockTimeout)
			defer cancel()
			if err := guard.Unlock(unlockCtx); err != nil {
				w.log.Warn("release prepared runtime warm lock failed", "error", err)
			}
		}()
	}
	var problems []error
	released, err := store.ReleaseExpiredPreparedRuntimeReservations(ctx, pgvalue.Timestamptz(time.Now()))
	if err != nil {
		problems = append(problems, err)
	} else if len(released) > 0 {
		w.log.Info("expired prepared runtime reservations released", "count", len(released))
	}
	superseded, err := store.CreateSupersededPreparedRuntimeStopCommands(ctx, w.limit)
	if err != nil {
		problems = append(problems, err)
	} else if len(superseded) > 0 {
		w.log.Info("superseded prepared runtime stop commands created", "count", len(superseded))
	}
	rows, err := store.ListRuntimeInstanceWarmTargets(ctx, db.ListRuntimeInstanceWarmTargetsParams{
		TargetCount:         w.targetCount,
		RowLimit:            w.limit,
		DeploymentSandboxID: deploymentSandboxID,
	})
	if err != nil {
		problems = append(problems, err)
		return errors.Join(problems...)
	}
	created := 0
	for _, row := range rows {
		source := preparedRuntimeSourceFromWarmTarget(row)
		key := runtime.KeyFromSource(source, compute.DefaultNetworkPolicy())
		instanceToken, err := runtime.NewInstanceToken()
		if err != nil {
			problems = append(problems, fmt.Errorf("generate prepared runtime instance token: %w", err))
			continue
		}
		instance, err := store.CreateRuntimeInstanceForDeploymentSandbox(ctx, db.CreateRuntimeInstanceForDeploymentSandboxParams{
			ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
			DeploymentSandboxID: row.DeploymentSandboxID,
			WorkerInstanceID:    row.WorkerInstanceID,
			RuntimeReleaseID:    row.RuntimeReleaseID,
			RootfsDigest:        row.RootfsDigest,
			RuntimeABI:          row.RuntimeABI,
			RuntimeKeyHash:      runtime.Hash(key),
			RuntimeKey:          []byte(key),
			InstanceToken:       instanceToken,
			ExpiresAt:           pgvalue.Timestamptz(time.Now().Add(defaultPreparedRuntimeInstanceTTL)),
		})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		payload, err := json.Marshal(api.WorkerRuntimePrepareCommand{
			DeploymentSandboxID: pgvalue.MustUUIDValue(row.DeploymentSandboxID).String(),
			RuntimeInstance:     preparedRuntimeWarmInstanceFromRow(instance),
			Source:              preparedRuntimeSourceFromWarmTarget(row),
		})
		if err != nil {
			w.markPrecreatedRuntimeFailed(ctx, store, instance, "encode prepared runtime warm command")
			problems = append(problems, fmt.Errorf("encode prepared runtime warm command: %w", err))
			continue
		}
		_, err = store.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
			OrgID:             row.OrgID,
			ProjectID:         row.ProjectID,
			EnvironmentID:     row.EnvironmentID,
			WorkerInstanceID:  row.WorkerInstanceID,
			RuntimeInstanceID: instance.ID,
			RuntimeEpoch:      pgtype.Int8{Int64: instance.RuntimeEpoch, Valid: true},
			RunStateVersion:   pgtype.Int8{},
			Kind:              db.WorkerCommandKindRuntimePrepare,
			Payload:           payload,
		})
		if err != nil {
			w.markPrecreatedRuntimeFailed(ctx, store, instance, "create prepared runtime warm command")
			problems = append(problems, err)
			continue
		}
		w.log.Info(
			"prepared runtime warm command created",
			"worker_instance_id", row.WorkerInstanceID,
			"deployment_sandbox_id", row.DeploymentSandboxID,
			"runtime_instance_id", instance.ID,
			"trigger_deployment_sandbox_id", deploymentSandboxID,
			"supply_count", row.SupplyCount,
			"pending_warm_count", row.CommandCount,
			"demand_count", row.DemandCount,
			"last_demand_at", row.LastDemandAt,
		)
		created++
	}
	createdSubstrates, err := w.prepareSubstrates(ctx, store)
	if err != nil {
		problems = append(problems, err)
	}
	if createdSubstrates > 0 || created > 0 {
		w.log.Info("prepared runtime warm commands created", "created", created, "substrate_prepare_created", createdSubstrates, "target_count", w.targetCount)
	}
	return errors.Join(problems...)
}

func (w *RuntimePreparer) markPrecreatedRuntimeFailed(ctx context.Context, store RuntimePreparerStore, row db.CreateRuntimeInstanceForDeploymentSandboxRow, message string) {
	_, err := store.MarkRuntimeInstanceFailed(ctx, db.MarkRuntimeInstanceFailedParams{
		Error:            fmt.Appendf(nil, `{"message":%q}`, message),
		ID:               row.ID,
		WorkerInstanceID: row.WorkerInstanceID,
		InstanceToken:    row.InstanceToken,
	})
	if err != nil {
		w.log.Warn("mark precreated prepared runtime failed", "runtime_instance_id", row.ID, "error", err)
	}
}

func (w *RuntimePreparer) prepareSubstrates(ctx context.Context, store RuntimePreparerStore) (int, error) {
	rows, err := store.ListRuntimeSubstratePrepareTargets(ctx, db.ListRuntimeSubstratePrepareTargetsParams{
		SubstrateFormat:     substrate.Format,
		SubstrateBuilderAbi: substrate.BuilderABI,
		SubstrateLayoutAbi:  substrate.LayoutABI,
		RowLimit:            w.limit,
	})
	if err != nil {
		return 0, err
	}
	var problems []error
	created := 0
	for _, row := range rows {
		payload, err := json.Marshal(api.WorkerRuntimeSubstratePrepareCommand{
			DeploymentSandboxID: pgvalue.MustUUIDValue(row.DeploymentSandboxID).String(),
			Source: api.WorkerPreparedRuntimeSource{
				DeploymentSandboxID: pgvalue.MustUUIDValue(row.DeploymentSandboxID).String(),
				RuntimeID:           row.RuntimeReleaseID,
				SandboxImageArtifact: api.CASObject{
					Digest:    row.SandboxImageArtifactDigest,
					MediaType: row.SandboxImageArtifactMediaType,
					SizeBytes: row.SandboxImageArtifactSizeBytes,
				},
				SandboxImageArtifactFormat: row.SandboxImageArtifactFormat,
				RootfsDigest:               row.RootfsDigest,
				ImageDigest:                row.ImageDigest,
				ImageFormat:                row.ImageFormat,
				WorkspaceMountPath:         row.WorkspaceMountPath,
				RuntimeABI:                 row.RuntimeABI,
				GuestdABI:                  row.GuestdAbi,
				AdapterABI:                 row.AdapterAbi,
			},
		})
		if err != nil {
			problems = append(problems, fmt.Errorf("encode runtime substrate prepare command: %w", err))
			continue
		}
		_, err = store.CreateWorkerCommand(ctx, db.CreateWorkerCommandParams{
			OrgID:               row.OrgID,
			ProjectID:           row.ProjectID,
			EnvironmentID:       row.EnvironmentID,
			WorkerInstanceID:    row.WorkerInstanceID,
			DeploymentSandboxID: row.DeploymentSandboxID,
			RunStateVersion:     pgtype.Int8{},
			Kind:                db.WorkerCommandKindRuntimeSubstratePrepare,
			Payload:             payload,
		})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		w.log.Info(
			"runtime substrate prepare command created",
			"worker_instance_id", row.WorkerInstanceID,
			"deployment_sandbox_id", row.DeploymentSandboxID,
			"demand_count", row.DemandCount,
			"last_demand_at", row.LastDemandAt,
		)
		created++
	}
	return created, errors.Join(problems...)
}

func preparedRuntimeSourceFromWarmTarget(row db.ListRuntimeInstanceWarmTargetsRow) api.WorkerPreparedRuntimeSource {
	return api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID: pgvalue.MustUUIDValue(row.DeploymentSandboxID).String(),
		RuntimeID:           row.RuntimeReleaseID,
		SandboxImageArtifact: api.CASObject{
			Digest:    row.SandboxImageArtifactDigest,
			MediaType: row.SandboxImageArtifactMediaType,
			SizeBytes: row.SandboxImageArtifactSizeBytes,
		},
		SandboxImageArtifactFormat: row.SandboxImageArtifactFormat,
		RootfsDigest:               row.RootfsDigest,
		ImageDigest:                row.ImageDigest,
		ImageFormat:                row.ImageFormat,
		WorkspaceMountPath:         row.WorkspaceMountPath,
		RuntimeABI:                 row.RuntimeABI,
		GuestdABI:                  row.GuestdAbi,
		AdapterABI:                 row.AdapterAbi,
	}
}

func preparedRuntimeWarmInstanceFromRow(row db.CreateRuntimeInstanceForDeploymentSandboxRow) api.WorkerRuntimeInstance {
	return api.WorkerRuntimeInstance{
		ID:                     pgvalue.MustUUIDValue(row.ID).String(),
		OrgID:                  pgvalue.MustUUIDValue(row.OrgID).String(),
		ProjectID:              pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:          pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		WorkerInstanceID:       pgvalue.MustUUIDValue(row.WorkerInstanceID).String(),
		RuntimeKeyHash:         row.RuntimeKeyHash,
		RuntimeKey:             json.RawMessage(row.RuntimeKey),
		RuntimeID:              row.RuntimeReleaseID,
		DeploymentSandboxID:    pgvalue.MustUUIDValue(row.DeploymentSandboxID).String(),
		State:                  string(row.State),
		InstanceToken:          row.InstanceToken,
		ReservedCpuMillis:      row.ReservedCpuMillis,
		ReservedMemoryMiB:      row.ReservedMemoryMib,
		ReservedDiskMiB:        row.ReservedDiskMib,
		ReservedExecutionSlots: row.ReservedExecutionSlots,
		WorkspaceMountID:       pgvalue.UUIDString(row.WorkspaceMountID),
		ExpiresAt:              optionalTimestamptz(row.ExpiresAt),
	}
}

func optionalTimestamptz(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
