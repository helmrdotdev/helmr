// Package dbpool opens Helmr's PostgreSQL application connections.
//
// Helmr's transactional correctness assumes READ COMMITTED. Every connection
// that runs application work or schema migrations, in production or tests, pins
// it through this package so a database- or role-level default cannot change
// it. Only administrative connections (role and database DDL) and one-off probe
// connections (server version checks) may open raw pgx connections.
package dbpool

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// New opens a PostgreSQL pool whose connections default every transaction to
// READ COMMITTED. config is copied and left unchanged.
func New(ctx context.Context, config *pgxpool.Config) (*pgxpool.Pool, error) {
	config = config.Copy()
	PinReadCommitted(config.ConnConfig)
	return pgxpool.NewWithConfig(ctx, config)
}

// PinReadCommitted sets READ COMMITTED as the session default transaction
// isolation of connections opened with config.
func PinReadCommitted(config *pgx.ConnConfig) {
	if config.RuntimeParams == nil {
		config.RuntimeParams = make(map[string]string, 1)
	}
	config.RuntimeParams["default_transaction_isolation"] = "read committed"
}
