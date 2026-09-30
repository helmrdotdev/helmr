// Package dbpool opens Helmr's PostgreSQL connection pools.
package dbpool

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// New opens a PostgreSQL pool whose connections default every transaction to
// READ COMMITTED. Helmr's transactional correctness assumes that isolation
// level, so it is pinned as a connection runtime parameter and a database- or
// role-level default cannot change it. config is copied and left unchanged.
func New(ctx context.Context, config *pgxpool.Config) (*pgxpool.Pool, error) {
	config = config.Copy()
	config.ConnConfig.RuntimeParams["default_transaction_isolation"] = "read committed"
	return pgxpool.NewWithConfig(ctx, config)
}
