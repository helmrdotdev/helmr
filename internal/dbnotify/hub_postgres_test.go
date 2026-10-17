package dbnotify

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestCommitRollbackAndListenerReconnect(t *testing.T) {
	database := dbtest.Open(t)
	h := New()
	wake, stop := h.Subscribe("session:example")
	defer stop()
	config := database.Pool.Config().ConnConfig.Copy()
	config.RuntimeParams["application_name"] = "notification-test"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx, config, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	defer func() { cancel(); <-done }()
	wait := func() {
		t.Helper()
		select {
		case <-wake:
		case <-time.After(5 * time.Second):
			t.Fatal("notification missing")
		}
	}
	wait() // LISTEN committed, snapshot catch-up permitted.
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := Notify(t.Context(), tx, "session:example"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
		t.Fatal("uncommitted transaction notified")
	case <-time.After(40 * time.Millisecond):
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
		t.Fatal("rollback notified")
	case <-time.After(40 * time.Millisecond):
	}
	if err := db.RunTx(t.Context(), database.Pool, func(tx pgx.Tx) error { return Notify(t.Context(), tx, "session:example") }); err != nil {
		t.Fatal(err)
	}
	wait()
	if _, err := database.Pool.Exec(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='notification-test'`); err != nil {
		t.Fatal(err)
	}
	wait() // Reconnected listener wakes even if all offline notifications were lost.
	if err := db.RunTx(t.Context(), database.Pool, func(tx pgx.Tx) error { return Notify(t.Context(), tx, "session:example") }); err != nil {
		t.Fatal(err)
	}
	wait()
}
