package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// These apply the computer owner's host binding checks before a Computer's,
// checkpoint's or Environment's rows are locked, reporting a target this host
// epoch does not hold as this owner's denial.

func requireHostLease(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, computerID uuid.UUID, epoch int64) error {
	return sessionDenial(computer.RequireHostLease(ctx, tx, host, env, computerID, epoch))
}

func requireHostCheckpointSource(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, checkpoint uuid.UUID) error {
	return sessionDenial(computer.RequireHostCheckpointSource(ctx, tx, host, env, checkpoint))
}

func requireHostCheckpointRestore(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, checkpoint uuid.UUID) error {
	return sessionDenial(computer.RequireHostCheckpointRestore(ctx, tx, host, env, checkpoint))
}

func requireHostCheckpointTarget(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, epoch int64) error {
	return sessionDenial(computer.RequireHostCheckpointTarget(ctx, tx, host, env, checkpoint, epoch))
}

func sessionDenial(err error) error {
	if errors.Is(err, computer.ErrDenied) {
		return ErrDenied
	}
	return err
}
