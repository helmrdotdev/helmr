package dispatch

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// These apply the computer owner's host binding checks before an allocation's
// rows are locked, reporting a target this host epoch does not hold as this
// owner's denial.

func requireHostLease(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, computerID uuid.UUID, epoch int64) error {
	return dispatchDenial(computer.RequireHostLease(ctx, tx, host, env, computerID, epoch))
}

func requireHostImageBuild(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, build uuid.UUID) error {
	return dispatchDenial(computer.RequireHostImageBuild(ctx, tx, host, env, build))
}

func requireHostBootSeedBuild(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, build uuid.UUID) error {
	return dispatchDenial(computer.RequireHostBootSeedBuild(ctx, tx, host, env, build))
}

func dispatchDenial(err error) error {
	if errors.Is(err, computer.ErrDenied) {
		return ErrDenied
	}
	return err
}
