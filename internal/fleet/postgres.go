package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresSource builds one repeatable-read snapshot per group. Queued work is
// attributed by the group's region; lease placement remains guarded by the
// exact runtime identity, certification and worker epoch in the lease path.
type PostgresSource struct {
	pool             *pgxpool.Pool
	role             string
	expectedCapacity Capacity
	queuedRunScratch uint64
	timeout          time.Duration
}

func NewPostgresSource(pool *pgxpool.Pool, role string, expected Capacity, queuedRunScratch uint64, timeout time.Duration) (*PostgresSource, error) {
	if pool == nil || (role != "run" && role != "build") || expected.isZero() || timeout <= 0 || (role == "run" && queuedRunScratch == 0) || (role == "build" && queuedRunScratch != 0) {
		return nil, errors.New("fleet Postgres source requires pool, run/build role, capacity, and positive timeout")
	}
	return &PostgresSource{pool: pool, role: role, expectedCapacity: expected, queuedRunScratch: queuedRunScratch, timeout: timeout}, nil
}

func (s *PostgresSource) Snapshot(ctx context.Context, groupID string) (GroupSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return GroupSnapshot{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(s.pool).WithTx(tx)

	var demand Demand
	uncertifiedRunLaunchAttestations := 0
	if s.role == "run" {
		uncovered, err := q.CountUncertifiedRunLaunchAttestations(ctx, groupID)
		if err != nil {
			return GroupSnapshot{}, err
		}
		rows, err := q.ListFleetRunDemand(ctx, groupID)
		if err != nil {
			return GroupSnapshot{}, err
		}
		for _, row := range rows {
			bucket, err := runDemandBucket(row, s.queuedRunScratch)
			if err != nil {
				return GroupSnapshot{}, err
			}
			appendDemandBucket(&demand, row.DemandState, bucket)
		}
		oldest, err := q.GetFleetOldestRunQueueTime(ctx, groupID)
		if err != nil {
			return GroupSnapshot{}, err
		}
		if oldest.Valid {
			demand.OldestQueuedAt = oldest.Time
		}
		uncertifiedRunLaunchAttestations = int(uncovered)
		if int64(uncertifiedRunLaunchAttestations) != uncovered {
			return GroupSnapshot{}, errors.New("uncertified run attestation count overflows int")
		}
	} else {
		rows, err := q.ListFleetBuildDemand(ctx, groupID)
		if err != nil {
			return GroupSnapshot{}, err
		}
		for _, row := range rows {
			bucket, err := buildDemandBucket(groupID, row)
			if err != nil {
				return GroupSnapshot{}, err
			}
			appendDemandBucket(&demand, row.DemandState, bucket)
		}
		oldest, err := q.GetFleetOldestBuildQueueTime(ctx, groupID)
		if err != nil {
			return GroupSnapshot{}, err
		}
		if oldest.Valid {
			demand.OldestQueuedAt = oldest.Time
		}
	}

	rows, err := q.ListFleetWorkers(ctx, groupID)
	if err != nil {
		return GroupSnapshot{}, err
	}
	snapshot := GroupSnapshot{Inputs: Inputs{Demand: demand, UncertifiedRunLaunchAttestations: uncertifiedRunLaunchAttestations}, ResourceIDs: make(map[string]string, len(rows))}
	cooldown, err := q.GetFleetCooldown(ctx, groupID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return GroupSnapshot{}, err
	}
	if cooldown.LastScaleOutAt.Valid {
		snapshot.Inputs.LastScaleOutAt = cooldown.LastScaleOutAt.Time
	}
	if cooldown.LastScaleInAt.Valid {
		snapshot.Inputs.LastScaleInAt = cooldown.LastScaleInAt.Time
	}
	drainStarted := make(map[string]time.Time)
	for _, row := range rows {
		workerID := uuid.UUID(row.ID.Bytes).String()
		proof, err := q.GetFleetTerminationProof(ctx, db.GetFleetTerminationProofParams{WorkerInstanceID: row.ID, WorkerGroupID: groupID})
		if err != nil {
			return GroupSnapshot{}, err
		}
		worker, err := s.mapWorker(row, proof)
		if err != nil {
			return GroupSnapshot{}, fmt.Errorf("worker %s: %w", workerID, err)
		}
		snapshot.Inputs.Workers = append(snapshot.Inputs.Workers, worker)
		snapshot.ResourceIDs[workerID] = row.ResourceID
		if worker.State == WorkerDraining || worker.State == WorkerDisabled {
			if row.DrainingAt.Valid && (snapshot.OldestDrainStartedAt.IsZero() || row.DrainingAt.Time.Before(snapshot.OldestDrainStartedAt)) {
				snapshot.OldestDrainStartedAt = row.DrainingAt.Time
			}
			if row.DrainingAt.Valid {
				drainStarted[workerID] = row.DrainingAt.Time
			}
		}
	}
	snapshot.Inputs.TerminationCandidateID = selectTerminationCandidate(snapshot.Inputs.Workers, drainStarted)
	if err := tx.Commit(ctx); err != nil {
		return GroupSnapshot{}, err
	}
	return snapshot, nil
}

func selectTerminationCandidate(workers []Worker, drainStarted map[string]time.Time) string {
	readyDisabled := make([]string, 0)
	draining := make([]Worker, 0)
	for _, worker := range workers {
		if worker.State == WorkerDisabled && worker.AuthorityCount == 0 && (worker.LocalCleanupComplete || worker.FencedForTermination) {
			readyDisabled = append(readyDisabled, worker.ID)
		}
		if worker.State == WorkerLost && worker.AuthorityCount == 0 && worker.FencedForTermination {
			readyDisabled = append(readyDisabled, worker.ID)
		}
		if worker.State == WorkerDraining {
			draining = append(draining, worker)
		}
	}
	if len(readyDisabled) > 0 {
		sort.Strings(readyDisabled)
		return readyDisabled[0]
	}
	sort.Slice(draining, func(left, right int) bool {
		leftAt, rightAt := drainStarted[draining[left].ID], drainStarted[draining[right].ID]
		if !leftAt.Equal(rightAt) {
			if leftAt.IsZero() {
				return false
			}
			if rightAt.IsZero() {
				return true
			}
			return leftAt.Before(rightAt)
		}
		return draining[left].ID < draining[right].ID
	})
	if len(draining) > 0 {
		return draining[0].ID
	}
	return ""
}

func (s *PostgresSource) MarkDraining(ctx context.Context, groupID, workerID string) error {
	id, err := parsePGUUID(workerID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_, err = db.New(s.pool).MarkFleetWorkerDraining(ctx, db.MarkFleetWorkerDrainingParams{
		WorkerInstanceID: id, WorkerGroupID: groupID, WorkerRole: s.role,
	})
	return err
}

func (s *PostgresSource) TerminationProof(ctx context.Context, groupID, workerID string) (TerminationProof, error) {
	id, err := parsePGUUID(workerID)
	if err != nil {
		return TerminationProof{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	row, err := db.New(s.pool).GetFleetTerminationProof(ctx, db.GetFleetTerminationProofParams{WorkerInstanceID: id, WorkerGroupID: groupID})
	if err != nil {
		return TerminationProof{}, err
	}
	state, err := mapWorkerState(row.State)
	if err != nil {
		return TerminationProof{}, err
	}
	return TerminationProof{WorkerID: workerID, ResourceID: row.ResourceID, State: state, AuthorityCount: uint64(row.AuthorityCount), LocalCleanupComplete: row.LocalCleanupComplete, FencedForTermination: row.FencedForTermination}, nil
}

func (s *PostgresSource) ClaimTermination(ctx context.Context, groupID, workerID string) (TerminationProof, error) {
	id, err := parsePGUUID(workerID)
	if err != nil {
		return TerminationProof{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	row, err := db.New(s.pool).ClaimFleetWorkerTermination(ctx, db.ClaimFleetWorkerTerminationParams{
		WorkerInstanceID: id,
		WorkerGroupID:    groupID,
	})
	if err != nil {
		return TerminationProof{}, err
	}
	state, err := mapWorkerState(row.State)
	if err != nil {
		return TerminationProof{}, err
	}
	return TerminationProof{
		WorkerID: workerID, ResourceID: row.ResourceID, State: state,
		AuthorityCount: uint64(row.AuthorityCount), LocalCleanupComplete: row.LocalCleanupComplete,
		FencedForTermination: row.FencedForTermination,
	}, nil
}

func (s *PostgresSource) RecordMutationIntent(ctx context.Context, groupID string, action ConfirmedAction) error {
	return s.recordCooldown(ctx, groupID, action)
}

func (s *PostgresSource) RecordConfirmed(ctx context.Context, groupID string, action ConfirmedAction) error {
	if action.Action != ActionFinishTermination {
		return s.recordCooldown(ctx, groupID, action)
	}
	id, err := parsePGUUID(action.WorkerID)
	if err != nil {
		return err
	}
	if action.ResourceID == "" {
		return errors.New("confirmed fleet termination requires provider resource ID")
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	if _, err := q.ConfirmFleetWorkerProviderTermination(ctx, db.ConfirmFleetWorkerProviderTerminationParams{
		WorkerInstanceID: id,
		WorkerGroupID:    groupID,
		ResourceID:       action.ResourceID,
	}); err != nil {
		return err
	}
	if _, err := q.RecordFleetScaleIn(ctx, db.RecordFleetScaleInParams{
		WorkerGroupID: groupID,
		ActionAt:      pgtype.Timestamptz{Time: action.At, Valid: true},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresSource) recordCooldown(ctx context.Context, groupID string, action ConfirmedAction) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	q := db.New(s.pool)
	switch action.Action {
	case ActionLaunch:
		_, err := q.RecordFleetScaleOut(ctx, db.RecordFleetScaleOutParams{WorkerGroupID: groupID, ActionAt: pgtype.Timestamptz{Time: action.At, Valid: true}})
		return err
	case ActionFinishTermination:
		_, err := q.RecordFleetScaleIn(ctx, db.RecordFleetScaleInParams{WorkerGroupID: groupID, ActionAt: pgtype.Timestamptz{Time: action.At, Valid: true}})
		return err
	default:
		return fmt.Errorf("unsupported fleet cooldown action %d", action.Action)
	}
}

func (s *PostgresSource) mapWorker(row db.ListFleetWorkersRow, proof db.GetFleetTerminationProofRow) (Worker, error) {
	state, err := mapWorkerState(row.State)
	if err != nil {
		return Worker{}, err
	}
	actual := Capacity{
		MilliCPU: positive(row.CertifiedCpuMillis), MemoryBytes: positive(row.CertifiedMemoryBytes),
		WorkloadDiskBytes: positive(row.CertifiedWorkloadDiskBytes), ScratchBytes: positive(row.CertifiedScratchBytes),
		BuildCacheBytes: positive(row.CertifiedBuildCacheBytes), ArtifactCacheBytes: positive(row.CertifiedArtifactCacheBytes),
		VMSlots: positive(int64(row.MaxVmSlots)), BuildExecutors: positive(int64(row.MaxBuildExecutors)),
	}
	if state == WorkerActive && !capacityAtLeast(actual, s.expectedCapacity) {
		return Worker{}, fmt.Errorf("certified capacity %#v is below configured instance capacity %#v", actual, s.expectedCapacity)
	}
	worker := Worker{ID: uuid.UUID(row.ID.Bytes).String(), State: state, AuthorityCount: positive(proof.AuthorityCount), LocalCleanupComplete: proof.LocalCleanupComplete, FencedForTermination: proof.FencedForTermination}
	if row.ActivatedAt.Valid {
		worker.ActivatedAt = row.ActivatedAt.Time
	}
	return worker, nil
}

func appendDemandBucket(demand *Demand, state string, bucket WorkloadBucket) {
	if state == "queued" {
		demand.Queued = append(demand.Queued, bucket)
	} else {
		demand.Active = append(demand.Active, bucket)
	}
}

func runDemandBucket(row db.ListFleetRunDemandRow, queuedScratch uint64) (WorkloadBucket, error) {
	values := []int64{row.MilliCpu, int64(row.MemoryBytes), int64(row.WorkloadDiskBytes), row.ScratchBytes, row.VmSlots, row.DemandCount}
	if err := requirePositiveDemand(values); err != nil {
		return WorkloadBucket{}, err
	}
	scratch := uint64(row.ScratchBytes)
	if row.DemandState == "queued" {
		scratch = queuedScratch
	}
	return WorkloadBucket{CompatibilityKey: row.CompatibilityKey, Count: uint64(row.DemandCount), Shape: Capacity{
		MilliCPU: uint64(row.MilliCpu), MemoryBytes: uint64(row.MemoryBytes), WorkloadDiskBytes: uint64(row.WorkloadDiskBytes),
		ScratchBytes: scratch, VMSlots: uint64(row.VmSlots),
	}}, nil
}

func buildDemandBucket(groupID string, row db.ListFleetBuildDemandRow) (WorkloadBucket, error) {
	values := []int64{row.MilliCpu, row.MemoryBytes, row.WorkloadDiskBytes, row.ScratchBytes, row.BuildCacheBytes, row.ArtifactCacheBytes, row.BuildExecutors, row.DemandCount}
	if err := requirePositiveDemand(values); err != nil {
		return WorkloadBucket{}, err
	}
	return WorkloadBucket{CompatibilityKey: groupID, Count: uint64(row.DemandCount), Shape: Capacity{
		MilliCPU: uint64(row.MilliCpu), MemoryBytes: uint64(row.MemoryBytes), WorkloadDiskBytes: uint64(row.WorkloadDiskBytes),
		ScratchBytes: uint64(row.ScratchBytes), BuildCacheBytes: uint64(row.BuildCacheBytes), ArtifactCacheBytes: uint64(row.ArtifactCacheBytes), BuildExecutors: uint64(row.BuildExecutors),
	}}, nil
}

func requirePositiveDemand(values []int64) error {
	for _, value := range values {
		if value < 0 {
			return errors.New("negative fleet demand value")
		}
	}
	if len(values) == 0 || values[len(values)-1] <= 0 {
		return errors.New("fleet demand count must be positive")
	}
	return nil
}

func positive(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func capacityAtLeast(actual, expected Capacity) bool {
	return actual.MilliCPU >= expected.MilliCPU && actual.MemoryBytes >= expected.MemoryBytes &&
		actual.WorkloadDiskBytes >= expected.WorkloadDiskBytes && actual.ScratchBytes >= expected.ScratchBytes &&
		actual.BuildCacheBytes >= expected.BuildCacheBytes && actual.ArtifactCacheBytes >= expected.ArtifactCacheBytes &&
		actual.VMSlots >= expected.VMSlots && actual.BuildExecutors >= expected.BuildExecutors
}

func mapWorkerState(state db.WorkerInstanceState) (WorkerState, error) {
	switch state {
	case db.WorkerInstanceStateRegistering:
		return WorkerPending, nil
	case db.WorkerInstanceStateActive:
		return WorkerActive, nil
	case db.WorkerInstanceStateDraining:
		return WorkerDraining, nil
	case db.WorkerInstanceStateDisabled:
		return WorkerDisabled, nil
	case db.WorkerInstanceStateLost:
		return WorkerLost, nil
	default:
		return 0, fmt.Errorf("unsupported worker state %q", state)
	}
}

func parsePGUUID(raw string) (pgtype.UUID, error) {
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: [16]byte(parsed), Valid: true}, nil
}

// PGLeaderElector holds one dedicated pool connection for the lifetime of an
// exact per-group advisory lease. The pool should be capped at two connections
// for independent run/build controller contention.
type PGLeaderElector struct{ pool *pgxpool.Pool }

func NewPGLeaderElector(pool *pgxpool.Pool) (*PGLeaderElector, error) {
	if pool == nil {
		return nil, errors.New("fleet leader pool is required")
	}
	return &PGLeaderElector{pool: pool}, nil
}

func (e *PGLeaderElector) TryAcquire(ctx context.Context, groupID string) (LeaderLease, bool, error) {
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, "helmr:fleet:"+groupID).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &pgLeaderLease{conn: conn, key: "helmr:fleet:" + groupID}, true, nil
}

type pgLeaderLease struct {
	conn *pgxpool.Conn
	key  string
}

func (l *pgLeaderLease) Release() error {
	if l == nil || l.conn == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var released bool
	err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, l.key).Scan(&released)
	if err != nil {
		conn := l.conn.Hijack()
		l.conn = nil
		_ = conn.Close(ctx)
		return err
	}
	l.conn.Release()
	l.conn = nil
	if !released {
		return errors.New("fleet advisory lease was not held")
	}
	return nil
}
