package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestRunTxDurability(t *testing.T) {
	pool := dbtest.Open(t).Pool
	dbtest.MustExec(t, t.Context(), pool, `CREATE TABLE transaction_probe (id integer PRIMARY KEY)`)
	for _, mode := range []string{"commit", "work_error", "cancel", "panic", "commit_error"} {
		t.Run(mode, func(t *testing.T) {
			dbtest.MustExec(t, t.Context(), pool, `TRUNCATE transaction_probe`)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			workErr := errors.New("work failed")
			var err error
			var recovered any
			var tx pgx.Tx
			func() {
				defer func() { recovered = recover() }()
				err = db.RunTx(ctx, pool, func(work pgx.Tx) error {
					tx = work
					if _, e := tx.Exec(ctx, `INSERT INTO transaction_probe VALUES (1)`); e != nil {
						return e
					}
					switch mode {
					case "work_error":
						return workErr
					case "cancel":
						cancel()
						return ctx.Err()
					case "panic":
						panic(workErr)
					case "commit_error":
						// A swallowed SQL failure leaves PostgreSQL's transaction aborted.
						_, e := tx.Exec(ctx, `INSERT INTO transaction_probe VALUES (1)`)
						if e == nil {
							t.Fatal("expected duplicate key error")
						}
					}
					return nil
				})
			}()
			switch mode {
			case "commit":
				if err != nil {
					t.Fatal(err)
				}
			case "work_error":
				if !errors.Is(err, workErr) {
					t.Fatalf("error = %v", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v", err)
				}
			case "panic":
				if recovered != workErr {
					t.Fatalf("panic = %v", recovered)
				}
			case "commit_error":
				if !errors.Is(err, pgx.ErrTxCommitRollback) {
					t.Fatalf("error = %v", err)
				}
			}
			if mode != "panic" && recovered != nil {
				t.Fatalf("unexpected panic: %v", recovered)
			}
			var count int
			if e := pool.QueryRow(t.Context(), `SELECT count(*) FROM transaction_probe`).Scan(&count); e != nil {
				t.Fatal(e)
			}
			expected := 0
			if mode == "commit" {
				expected = 1
			}
			if count != expected {
				t.Fatalf("persisted rows = %d, want %d", count, expected)
			}
			if _, e := tx.Exec(t.Context(), `SELECT 1`); !errors.Is(e, pgx.ErrTxClosed) {
				t.Fatalf("transaction still open: %v", e)
			}
		})
	}
}
