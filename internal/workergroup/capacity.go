package workergroup

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// MaxResourceIDBytes bounds a worker host's provider resource ID.
const MaxResourceIDBytes = 512

// HostFilter selects worker hosts for the capacity protocol. A zero GroupID
// selects every group, and empty ResourceIDs or Statuses do not filter.
type HostFilter struct {
	GroupID                uuid.UUID
	ResourceIDs            []string
	Statuses               []WorkerHostStatus
	HasUnreclaimedInstance bool
	Limit                  int32
}

// ResolveGroup returns the capacity projection of the worker group with the
// name in the region.
func ResolveGroup(ctx context.Context, q db.Querier, regionID string, name string) (Group, error) {
	group, err := q.GetWorkerGroupByRegionName(ctx, db.GetWorkerGroupByRegionNameParams{RegionID: regionID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, ErrGroupNotFound
	}
	if err != nil {
		return Group{}, fmt.Errorf("resolve worker group %q in region %q: %w", name, regionID, err)
	}
	return projectGroup(group)
}

// ResolvePool returns the capacity projection of the worker group's pool with
// the name.
func ResolvePool(ctx context.Context, q db.Querier, groupID uuid.UUID, name string) (WorkerPool, error) {
	pool, err := q.GetWorkerPoolByGroupName(ctx, db.GetWorkerPoolByGroupNameParams{
		WorkerGroupID: pgvalue.UUID(groupID), Name: name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerPool{}, ErrPoolNotFound
	}
	if err != nil {
		return WorkerPool{}, fmt.Errorf("resolve worker pool %q in worker group %s: %w", name, groupID.String(), err)
	}
	status, err := publicPoolStatus(pool.Status)
	if err != nil {
		return WorkerPool{}, err
	}
	return WorkerPool{
		ID: pgvalue.UUIDString(pool.ID), WorkerGroupID: pgvalue.UUIDString(pool.WorkerGroupID),
		Name: pool.Name, Status: status,
	}, nil
}

// ReconcilePrimaryPools selects the provider controller's primary pool through
// SelectPrimaryPool and returns the group's capacity projection. A zero poolID
// is rejected as SelectPrimaryPool describes.
func ReconcilePrimaryPools(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, poolID uuid.UUID, expectedGroupClaimVersion int64) (ReconcilePrimaryPoolsResponse, error) {
	selection, err := SelectPrimaryPool(ctx, txb, groupID, poolID, expectedGroupClaimVersion)
	if err != nil {
		return ReconcilePrimaryPoolsResponse{}, err
	}
	group, err := projectGroup(selection.Group)
	if err != nil {
		return ReconcilePrimaryPoolsResponse{}, err
	}
	return ReconcilePrimaryPoolsResponse{WorkerGroup: group, Applied: selection.Applied}, nil
}

// ListHosts returns the worker hosts that match filter.
func ListHosts(ctx context.Context, q db.Querier, filter HostFilter) (ListWorkerHostsResponse, error) {
	params := db.ListCapacityWorkerHostsParams{
		ResourceIds:            append([]string{}, filter.ResourceIDs...),
		Statuses:               make([]string, 0, len(filter.Statuses)),
		HasUnreclaimedInstance: filter.HasUnreclaimedInstance,
		RowLimit:               filter.Limit,
	}
	if filter.GroupID != uuid.Nil() {
		params.WorkerGroupID = pgvalue.UUID(filter.GroupID)
	}
	for _, status := range filter.Statuses {
		params.Statuses = append(params.Statuses, string(status))
	}
	rows, err := q.ListCapacityWorkerHosts(ctx, params)
	if err != nil {
		return ListWorkerHostsResponse{}, fmt.Errorf("list capacity worker hosts: %w", err)
	}
	response := ListWorkerHostsResponse{WorkerHosts: make([]WorkerHost, 0, len(rows))}
	for _, row := range rows {
		host, err := projectHost(db.GetCapacityWorkerHostRow(row))
		if err != nil {
			return ListWorkerHostsResponse{}, err
		}
		response.WorkerHosts = append(response.WorkerHosts, host)
	}
	return response, nil
}

// GetHost returns one worker host.
func GetHost(ctx context.Context, q db.Querier, hostID uuid.UUID) (WorkerHost, error) {
	row, err := getCapacityHost(ctx, q, hostID)
	if err != nil {
		return WorkerHost{}, err
	}
	return projectHost(row)
}

// DrainHost starts the provider-requested drain of an active worker host fenced
// by its epoch and claim version. With RequireZeroQueuedDemand, an active host
// is not drained while its group's region has queued demand.
func DrainHost(ctx context.Context, q db.Querier, hostID uuid.UUID, request DrainWorkerHostRequest) (WorkerHost, error) {
	if request.ExpectedEpoch <= 0 || request.ExpectedClaimVersion <= 0 {
		return WorkerHost{}, invalidInput("expected_epoch and expected_claim_version must be positive")
	}
	host, err := getCapacityHost(ctx, q, hostID)
	if err != nil {
		return WorkerHost{}, err
	}
	if request.RequireZeroQueuedDemand && host.Status == string(db.WorkerHostStatusActive) {
		present, err := HasQueuedDemand(ctx, q, pgvalue.MustUUIDValue(host.WorkerGroupID))
		if err != nil {
			return WorkerHost{}, fmt.Errorf("check queued demand for worker host %s drain: %w", hostID.String(), err)
		}
		if present {
			return WorkerHost{}, ErrQueuedDemand
		}
	}
	drained, err := q.DrainWorkerHost(ctx, db.DrainWorkerHostParams{
		ID:                   pgvalue.UUID(hostID),
		WorkerGroupID:        host.WorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: request.ExpectedEpoch, Valid: true},
		ExpectedClaimVersion: request.ExpectedClaimVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerHost{}, conflict("worker drain fence is stale or the worker host is not active")
	}
	if err != nil {
		return WorkerHost{}, fmt.Errorf("drain worker host %s: %w", hostID.String(), err)
	}
	return projectHost(db.GetCapacityWorkerHostRow{
		ID: drained.ID, ResourceID: drained.ResourceID, WorkerGroupID: drained.WorkerGroupID,
		WorkerPoolID: drained.WorkerPoolID, Status: drained.Status, ClaimVersion: drained.ClaimVersion,
		CurrentEpoch: drained.CurrentEpoch, DrainingAt: drained.DrainingAt,
		TerminationReadyAt: drained.TerminationReadyAt, LostAt: drained.LostAt,
		CreatedAt: drained.CreatedAt, UpdatedAt: drained.UpdatedAt,
	})
}

// ConfirmHostProviderAbsent records the provider's confirmation that a worker
// host no longer exists and reconciles its Computer instances in one
// transaction. The host's identity is read before the transaction and
// compared with the confirmed row after commit; a mismatch is reported as an
// error after the confirmation has committed.
func ConfirmHostProviderAbsent(ctx context.Context, q db.Querier, txb db.TxBeginner, hostID uuid.UUID) (WorkerHost, error) {
	host, err := getCapacityHost(ctx, q, hostID)
	if err != nil {
		return WorkerHost{}, err
	}
	var confirmed db.ConfirmWorkerHostProviderAbsentRow
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		confirmed, err = q.ConfirmWorkerHostProviderAbsent(ctx, pgvalue.UUID(hostID))
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("worker host cannot be marked lost from its current state")
		}
		if err != nil {
			return fmt.Errorf("confirm worker host %s provider absence: %w", hostID.String(), err)
		}
		if _, err := q.ReconcileProviderAbsentWorkerInstances(ctx, pgvalue.UUID(hostID)); err != nil {
			return fmt.Errorf("reconcile provider-absent worker host %s instances: %w", hostID.String(), err)
		}
		return nil
	})
	if err != nil {
		return WorkerHost{}, err
	}
	if confirmed.WorkerGroupID != host.WorkerGroupID || confirmed.WorkerPoolID != host.WorkerPoolID || confirmed.ResourceID != host.ResourceID {
		return WorkerHost{}, fmt.Errorf("provider absence changed worker host %s identity", hostID.String())
	}
	return projectHost(db.GetCapacityWorkerHostRow(confirmed))
}

func getCapacityHost(ctx context.Context, q db.Querier, hostID uuid.UUID) (db.GetCapacityWorkerHostRow, error) {
	row, err := q.GetCapacityWorkerHost(ctx, pgvalue.UUID(hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetCapacityWorkerHostRow{}, ErrHostNotFound
	}
	if err != nil {
		return db.GetCapacityWorkerHostRow{}, fmt.Errorf("get worker host %s: %w", hostID.String(), err)
	}
	return row, nil
}

func projectGroup(group db.WorkerGroup) (Group, error) {
	status, err := publicGroupStatus(group.Status)
	if err != nil {
		return Group{}, err
	}
	return Group{
		ID: pgvalue.UUIDString(group.ID), Name: group.Name, RegionID: group.RegionID, Status: status,
		ClaimVersion:  group.ClaimVersion,
		PrimaryPoolID: pgvalue.UUIDString(group.PrimaryPoolID),
	}, nil
}

func projectHost(row db.GetCapacityWorkerHostRow) (WorkerHost, error) {
	status, err := publicHostStatus(row.Status)
	if err != nil {
		return WorkerHost{}, err
	}
	host := WorkerHost{
		ID: pgvalue.UUIDString(row.ID), ResourceID: row.ResourceID,
		WorkerGroupID: pgvalue.UUIDString(row.WorkerGroupID), WorkerPoolID: pgvalue.UUIDString(row.WorkerPoolID),
		Status: status, ClaimVersion: row.ClaimVersion,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
	if row.CurrentEpoch.Valid {
		host.CurrentEpoch = &row.CurrentEpoch.Int64
	}
	if row.DrainingAt.Valid {
		host.DrainingAt = &row.DrainingAt.Time
	}
	if row.TerminationReadyAt.Valid {
		host.TerminationReadyAt = &row.TerminationReadyAt.Time
	}
	if row.LostAt.Valid {
		host.LostAt = &row.LostAt.Time
	}
	return host, nil
}

func publicGroupStatus(state string) (WorkerGroupStatus, error) {
	switch state {
	case db.WorkerGroupStatusActive:
		return WorkerGroupStatusActive, nil
	case db.WorkerGroupStatusPaused:
		return WorkerGroupStatusPaused, nil
	case db.WorkerGroupStatusDraining:
		return WorkerGroupStatusDraining, nil
	case db.WorkerGroupStatusDisabled:
		return WorkerGroupStatusDisabled, nil
	default:
		return "", fmt.Errorf("worker group state %q has no public projection", state)
	}
}

func publicPoolStatus(state string) (WorkerPoolStatus, error) {
	switch state {
	case "pending":
		return WorkerPoolStatusPending, nil
	case "active":
		return WorkerPoolStatusActive, nil
	case "draining":
		return WorkerPoolStatusDraining, nil
	case "disabled":
		return WorkerPoolStatusDisabled, nil
	default:
		return "", fmt.Errorf("worker pool state %q has no public projection", state)
	}
}

func publicHostStatus(state string) (WorkerHostStatus, error) {
	switch state {
	case db.WorkerHostStatusRegistering:
		return WorkerHostStatusRegistering, nil
	case db.WorkerHostStatusActive:
		return WorkerHostStatusActive, nil
	case db.WorkerHostStatusDraining:
		return WorkerHostStatusDraining, nil
	case db.WorkerHostStatusTerminationReady:
		return WorkerHostStatusTerminationReady, nil
	case db.WorkerHostStatusLost:
		return WorkerHostStatusLost, nil
	default:
		return "", fmt.Errorf("worker host state %q has no public projection", state)
	}
}
