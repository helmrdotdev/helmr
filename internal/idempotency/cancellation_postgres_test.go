package idempotency_test

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	retry "github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
	"testing"
)

func TestPlatformUnkeyedCancellationCannotCommit(t *testing.T) {
	f := sessiontest.New(t)
	var innerErr error
	err := db.RunTx(t.Context(), f.Pool, func(outer pgx.Tx) error {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		innerErr = retry.Run(ctx, outer, nil, func(tx pgx.Tx, r retry.Result) (retry.Completion, error) {
			_, err := tx.Exec(ctx, `UPDATE computers SET key='partial' WHERE environment_id=$1 AND id=$2`, f.Environment, f.Computer)
			if err != nil {
				return retry.Completion{}, err
			}
			cancel()
			return retry.Completion{}, nil
		})
		return nil // The API promises this cannot commit cancelled inner writes.
	})
	if err != nil {
		t.Fatalf("outer: %v", err)
	}
	t.Logf("inner error: %v", innerErr)
	assertNoPartialMutation(t, f)
}

type releaseCancelTracer struct{ cancel context.CancelFunc }

func (tr *releaseCancelTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (tr *releaseCancelTracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil && data.CommandTag.String() == "RELEASE" && tr.cancel != nil {
		tr.cancel()
		tr.cancel = nil
	}
}

func TestPlatformPublicSecretCancellationCannotCommit(t *testing.T) {
	f := sessiontest.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	config := f.Pool.Config().ConnConfig.Copy()
	config.Tracer = &releaseCancelTracer{cancel: cancel}
	conn, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	outer, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Rollback(t.Context())
	store, err := secret.New(db.New(outer), outer, make([]byte, 32), auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	_, innerErr := store.Create(ctx, audit.System("test-fixture"), f.Environment, "CANCELLED_SECRET", []byte("value"), "cancelled-secret")
	if innerErr == nil {
		t.Fatal("wanted cancellation error")
	}
	t.Logf("public Secret.Create returned: %v", innerErr)
	if err := outer.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM secrets WHERE environment_id=$1 AND name='CANCELLED_SECRET'`, f.Environment).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cancelled Secret.Create committed %d Secret", count)
	}
}
