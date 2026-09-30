package dbpool_test

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNewPinsReadCommittedOverRoleDefault(t *testing.T) {
	ctx := t.Context()
	database := dbtest.Open(t)
	role := "helmr_isolation_" + rand.Text()
	password := rand.Text()
	roleIdentifier := pgx.Identifier{role}.Sanitize()
	dbtest.MustExec(t, ctx, database.Pool, "CREATE ROLE "+roleIdentifier+" LOGIN PASSWORD '"+password+"'")
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+roleIdentifier)
	})
	dbtest.MustExec(t, ctx, database.Pool, "ALTER ROLE "+roleIdentifier+" SET default_transaction_isolation = 'repeatable read'")

	config, err := pgxpool.ParseConfig(database.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.User = role
	config.ConnConfig.Password = password

	unpinned, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer unpinned.Close()
	if got := transactionIsolation(t, unpinned); got != "repeatable read" {
		t.Fatalf("role default isolation = %q, want the override to be effective", got)
	}

	pool, err := dbpool.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := transactionIsolation(t, pool); got != "read committed" {
		t.Fatalf("pool transaction isolation = %q, want read committed", got)
	}
	if _, ok := config.ConnConfig.RuntimeParams["default_transaction_isolation"]; ok {
		t.Fatal("New modified the caller's config")
	}
}

func transactionIsolation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var isolation string
	if err := tx.QueryRow(t.Context(), "SHOW transaction_isolation").Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	return isolation
}
