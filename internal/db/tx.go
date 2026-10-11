package db

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TxBeginner starts the PostgreSQL transactions that RunTx owns.
type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// TxDB runs direct statements and starts the transactions that RunTx owns.
type TxDB interface {
	DBTX
	TxBeginner
}

type txLifecycleError struct {
	stage string
	err   error
}

func (e txLifecycleError) Error() string {
	return e.stage
}

// LogValue exposes typed failure categories without connection strings, SQL or
// server error text. Error remains stage-only for non-structured consumers.
func (e txLifecycleError) LogValue() slog.Value {
	kind, state := "unknown", ""
	var postgres *pgconn.PgError
	var network net.Error
	switch {
	case errors.Is(e.err, context.Canceled):
		kind = "context_cancelled"
	case errors.Is(e.err, context.DeadlineExceeded):
		kind = "deadline_exceeded"
	case errors.Is(e.err, pgx.ErrTxClosed):
		kind = "transaction_closed"
	case errors.Is(e.err, pgx.ErrTxCommitRollback):
		kind = "transaction_rolled_back"
	case errors.Is(e.err, io.EOF), errors.Is(e.err, io.ErrUnexpectedEOF):
		kind = "connection_closed"
	case errors.As(e.err, &postgres):
		kind = "postgres"
		// SQLSTATE is exactly five uppercase ASCII letters or digits.
		if len(postgres.Code) == 5 {
			valid := true
			for _, c := range postgres.Code {
				valid = valid && (c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
			}
			if valid {
				state = postgres.Code
			}
		}
	case errors.As(e.err, &network):
		kind = "network"
		if network.Timeout() {
			kind = "network_timeout"
		}
	}
	return slog.GroupValue(slog.String("stage", e.stage), slog.String("cause", kind), slog.String("sqlstate", state))
}

func (e txLifecycleError) Unwrap() error {
	return e.err
}

func txError(stage string, err error) error {
	if err == nil {
		return nil
	}
	return txLifecycleError{stage: stage, err: err}
}

// errors.Join preserves identity but hides the joined errors' LogValue methods.
// Keep both transaction stages inspectable without logging either raw cause.
type txRollbackFailure struct {
	primary  error
	rollback error
}

func (e txRollbackFailure) Error() string {
	return e.primary.Error() + "\n" + e.rollback.Error()
}

func (e txRollbackFailure) Unwrap() []error {
	return []error{e.primary, e.rollback}
}

func (e txRollbackFailure) LogValue() slog.Value {
	var operation txLifecycleError
	if !errors.As(e.primary, &operation) {
		operation = txLifecycleError{stage: "transaction body", err: e.primary}
	}
	return slog.GroupValue(slog.Any("operation", operation), slog.Any("rollback", e.rollback))
}

func withRollbackFailure(primary, rollback error) error {
	if rollback == nil {
		return primary
	}
	rollback = txError("rollback transaction", rollback)
	if primary == nil {
		return rollback
	}
	return txRollbackFailure{primary: primary, rollback: rollback}
}

// RunTx runs fn in one PostgreSQL transaction. It commits when fn returns nil
// and rolls back when fn returns an error, panics or the commit fails; a panic
// is re-raised after rollback. Lifecycle errors keep their cause for errors.Is
// but report only the transaction stage in their text.
func RunTx(ctx context.Context, txb TxBeginner, fn func(pgx.Tx) error) error {
	if fn == nil {
		return errors.New("transaction function is required")
	}
	if txb == nil {
		return errors.New("transactional database is required")
	}
	tx, err := txb.Begin(ctx)
	if err != nil {
		return txError("begin transaction", err)
	}
	return runTransaction(ctx, tx, fn)
}

func runTransaction(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) (err error) {
	if tx == nil {
		return errors.New("transaction is required")
	}
	committed := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if !committed {
				err = withRollbackFailure(err, tx.Rollback(context.WithoutCancel(ctx)))
			}
			panic(recovered)
		}
		if err != nil && !committed {
			err = withRollbackFailure(err, tx.Rollback(context.WithoutCancel(ctx)))
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return txError("commit transaction", err)
	}
	committed = true
	return nil
}
