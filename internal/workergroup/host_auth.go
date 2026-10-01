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
// host secret record, epoch and claim versions it names in one statement for
// the host states its routes admit. A host credential that does not
// authenticate returns ErrUnauthenticated; any other error means
// authorization could not be decided.

// AuthenticateHost authenticates a worker host for its ordinary routes.
func AuthenticateHost(ctx context.Context, q db.Querier, cfg CredentialConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, q.AuthorizeWorkerHostCredential)
}

// AuthenticateActivatingHost authenticates a worker host that is activating
// its epoch.
func AuthenticateActivatingHost(ctx context.Context, q db.Querier, cfg CredentialConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, func(ctx context.Context, params db.AuthorizeWorkerHostCredentialParams) (db.AuthorizeWorkerHostCredentialRow, error) {
		row, err := q.AuthorizeWorkerActivationCredential(ctx, db.AuthorizeWorkerActivationCredentialParams(params))
		return db.AuthorizeWorkerHostCredentialRow(row), err
	})
}

// AuthenticateRecoveringHost authenticates a worker host that is reporting
// its startup recovery.
func AuthenticateRecoveringHost(ctx context.Context, q db.Querier, cfg CredentialConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, func(ctx context.Context, params db.AuthorizeWorkerHostCredentialParams) (db.AuthorizeWorkerHostCredentialRow, error) {
		row, err := q.AuthorizeRecoveringWorkerHostCredential(ctx, db.AuthorizeRecoveringWorkerHostCredentialParams(params))
		return db.AuthorizeWorkerHostCredentialRow(row), err
	})
}

// AuthenticateDrainCompletingHost authenticates a worker host that completes
// its drain, including a replay after the completion was recorded.
func AuthenticateDrainCompletingHost(ctx context.Context, q db.Querier, cfg CredentialConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, func(ctx context.Context, params db.AuthorizeWorkerHostCredentialParams) (db.AuthorizeWorkerHostCredentialRow, error) {
		row, err := q.AuthorizeWorkerHostCredential(ctx, params)
		if !errors.Is(err, pgx.ErrNoRows) {
			return row, err
		}
		replay, err := q.AuthorizeWorkerDrainReplay(ctx, db.AuthorizeWorkerDrainReplayParams{
			CredentialID: params.CredentialID, ClaimVersion: params.ClaimVersion,
			WorkerEpoch: params.WorkerEpoch,
		})
		return db.AuthorizeWorkerHostCredentialRow(replay), err
	})
}

// AuthenticateFencingHost authenticates a worker host that fences itself,
// including a replay after the fence was recorded.
func AuthenticateFencingHost(ctx context.Context, q db.Querier, cfg CredentialConfig, rawCredential string, now time.Time) (HostPrincipal, error) {
	return authenticateHost(ctx, q, cfg, rawCredential, now, func(ctx context.Context, params db.AuthorizeWorkerHostCredentialParams) (db.AuthorizeWorkerHostCredentialRow, error) {
		row, err := q.AuthorizeWorkerHostCredential(ctx, params)
		if !errors.Is(err, pgx.ErrNoRows) {
			return row, err
		}
		replay, err := q.AuthorizeWorkerFenceReplay(ctx, db.AuthorizeWorkerFenceReplayParams{
			CredentialID: params.CredentialID, ClaimVersion: params.ClaimVersion,
			WorkerEpoch: params.WorkerEpoch,
		})
		return db.AuthorizeWorkerHostCredentialRow(replay), err
	})
}

type hostAuthorization func(context.Context, db.AuthorizeWorkerHostCredentialParams) (db.AuthorizeWorkerHostCredentialRow, error)

func authenticateHost(ctx context.Context, q db.Querier, cfg CredentialConfig, rawCredential string, now time.Time, authorize hostAuthorization) (HostPrincipal, error) {
	payload, err := verifyHostCredential(cfg.signingKey, rawCredential, now)
	if err != nil {
		return HostPrincipal{}, ErrUnauthenticated
	}
	credentialID, err := uuid.Parse(payload.CredentialID)
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
	row, err := authorize(ctx, db.AuthorizeWorkerHostCredentialParams{
		CredentialID:      pgvalue.UUID(credentialID),
		ClaimVersion:      payload.ClaimVersion,
		GroupClaimVersion: payload.GroupClaimVersion,
		WorkerEpoch:       pgtype.Int8{Int64: payload.WorkerEpoch, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return HostPrincipal{}, ErrUnauthenticated
	}
	if err != nil {
		return HostPrincipal{}, fmt.Errorf("authorize worker host %s credential: %w", payload.WorkerHostID, err)
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
	if pgvalue.MustUUIDValue(row.WorkerHostID) != hostID || principal.GroupID != groupID || payload.ClaimVersion != principal.HostClaimVersion {
		return HostPrincipal{}, ErrUnauthenticated
	}
	return principal, nil
}
