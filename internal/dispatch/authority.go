package dispatch

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNilPool             = errors.New("dispatch: nil pgx pool")
	ErrCapacityUnavailable = errors.New("dispatch: ready capacity unavailable")
	ErrCandidateChanged    = errors.New("dispatch: placement candidate changed while locking")
)

const runtimeArchitecture = "x86_64"

type Authority struct {
	pool       *pgxpool.Pool
	fencingKey disk.FencingKey
}

func NewRunAuthority(
	pool *pgxpool.Pool,
	fencingKey disk.FencingKey,
) (*Authority, error) {
	authority, err := newAuthority(pool)
	if err != nil {
		return nil, err
	}
	if !fencingKey.Valid() {
		return nil, errors.New("run authority computer fencing key is required")
	}
	authority.fencingKey = fencingKey
	return authority, nil
}

func newAuthority(pool *pgxpool.Pool) (*Authority, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	return &Authority{pool: pool}, nil
}

func (d *Authority) begin(ctx context.Context) (pgx.Tx, error) {
	// Dispatch authority transactions lock each mutable scope explicitly. READ
	// COMMITTED lets a statement that follows a blocking scope or Worker lock
	// re-read the state committed by the previous owner before it applies new
	// authority.
	return d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}

func rollback(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(ctx)
}
