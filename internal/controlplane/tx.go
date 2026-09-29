package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type txWork struct {
	q  db.Querier
	tx pgx.Tx
}

func inTxWith(ctx context.Context, txb db.TxBeginner, fn func(*txWork) error) error {
	if fn == nil {
		return db.RunTx(ctx, txb, nil)
	}
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		return fn(&txWork{q: db.New(tx), tx: tx})
	})
}
