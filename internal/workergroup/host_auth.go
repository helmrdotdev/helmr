package workergroup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Each authenticator verifies a worker host credential and authorizes the
// host key record, epoch and claim versions it names in one statement for
// the host states its routes admit. A host credential that does not
// authenticate returns ErrUnauthenticated; any other error means
// authorization could not be decided.

// AuthenticateHost authenticates a worker host for its ordinary routes.
func AuthenticateHost(ctx context.Context, q db.Querier, cfg HostAuthConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, q.AuthorizeWorkerHostKey)
}

// AuthenticateActivatingHost authenticates a worker host that is activating
// its epoch.
func AuthenticateActivatingHost(ctx context.Context, q db.Querier, cfg HostAuthConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, func(ctx context.Context, params db.AuthorizeWorkerHostKeyParams) (db.AuthorizeWorkerHostKeyRow, error) {
		row, err := q.AuthorizeActivatingWorkerHostKey(ctx, db.AuthorizeActivatingWorkerHostKeyParams(params))
		return db.AuthorizeWorkerHostKeyRow(row), err
	})
}

// AuthenticateRecoveringHost authenticates a worker host that is reporting
// its startup recovery.
func AuthenticateRecoveringHost(ctx context.Context, q db.Querier, cfg HostAuthConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, func(ctx context.Context, params db.AuthorizeWorkerHostKeyParams) (db.AuthorizeWorkerHostKeyRow, error) {
		row, err := q.AuthorizeRecoveringWorkerHostKey(ctx, db.AuthorizeRecoveringWorkerHostKeyParams(params))
		return db.AuthorizeWorkerHostKeyRow(row), err
	})
}

// AuthenticateDrainCompletingHost authenticates a worker host that completes
// its drain, including a replay after the completion was recorded.
func AuthenticateDrainCompletingHost(ctx context.Context, q db.Querier, cfg HostAuthConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, func(ctx context.Context, params db.AuthorizeWorkerHostKeyParams) (db.AuthorizeWorkerHostKeyRow, error) {
		row, err := q.AuthorizeWorkerHostKey(ctx, params)
		if !errors.Is(err, pgx.ErrNoRows) {
			return row, err
		}
		replay, err := q.AuthorizeWorkerDrainReplay(ctx, db.AuthorizeWorkerDrainReplayParams{
			HostKeyID: params.HostKeyID, ClaimVersion: params.ClaimVersion,
			WorkerEpoch: params.WorkerEpoch,
		})
		return db.AuthorizeWorkerHostKeyRow(replay), err
	})
}

// AuthenticateFencingHost authenticates a worker host that fences itself,
// including a replay after the fence was recorded.
func AuthenticateFencingHost(ctx context.Context, q db.Querier, cfg HostAuthConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, func(ctx context.Context, params db.AuthorizeWorkerHostKeyParams) (db.AuthorizeWorkerHostKeyRow, error) {
		row, err := q.AuthorizeWorkerHostKey(ctx, params)
		if !errors.Is(err, pgx.ErrNoRows) {
			return row, err
		}
		replay, err := q.AuthorizeWorkerFenceReplay(ctx, db.AuthorizeWorkerFenceReplayParams{
			HostKeyID: params.HostKeyID, ClaimVersion: params.ClaimVersion,
			WorkerEpoch: params.WorkerEpoch,
		})
		return db.AuthorizeWorkerHostKeyRow(replay), err
	})
}

type hostAuthorization func(context.Context, db.AuthorizeWorkerHostKeyParams) (db.AuthorizeWorkerHostKeyRow, error)

func authenticateHost(ctx context.Context, q db.Querier, cfg HostAuthConfig, rawCredential string, now time.Time, authorize hostAuthorization) (HostPrincipal, error) {
	payload, err := verifyHostCredential(cfg.signingKey, rawCredential, now)
	if err != nil {
		return HostPrincipal{}, ErrUnauthenticated
	}
	hostKeyID, err := uuid.Parse(payload.HostKeyID)
	if err != nil {
		return HostPrincipal{}, ErrUnauthenticated
	}
	hostID, err := uuid.Parse(payload.WorkerHostID)
	if err != nil {
		return HostPrincipal{}, ErrUnauthenticated
	}
	groupID, err := ids.Parse(payload.WorkerGroupID)
	if err != nil {
		return HostPrincipal{}, ErrUnauthenticated
	}
	row, err := authorize(ctx, db.AuthorizeWorkerHostKeyParams{
		HostKeyID:         pgvalue.UUID(hostKeyID),
		ClaimVersion:      payload.ClaimVersion,
		GroupClaimVersion: payload.GroupClaimVersion,
		WorkerEpoch:       pgtype.Int8{Int64: payload.WorkerEpoch, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return HostPrincipal{}, ErrUnauthenticated
	}
	if err != nil {
		return HostPrincipal{}, fmt.Errorf("authorize worker host %s secret: %w", payload.WorkerHostID, err)
	}
	principal := HostPrincipal{
		HostID:            hostID,
		GroupID:           pgvalue.MustUUIDValue(row.WorkerGroupID),
		Epoch:             payload.WorkerEpoch,
		HostClaimVersion:  row.ClaimVersion,
		GroupClaimVersion: payload.GroupClaimVersion,
		ResourceID:        strings.TrimSpace(row.ResourceID),
		Status:            row.WorkerStatus,
		EpochStartedAt:    pgvalue.Time(row.EpochStartedAt),
	}
	if row.WorkerGroupOwnerOrgID.Valid {
		owner := pgvalue.MustUUIDValue(row.WorkerGroupOwnerOrgID)
		principal.OwnerOrgID = &owner
	}
	if pgvalue.MustUUIDValue(row.WorkerHostID) != hostID || principal.GroupID != groupID || payload.ClaimVersion != principal.HostClaimVersion {
		return HostPrincipal{}, ErrUnauthenticated
	}
	return principal, nil
}
