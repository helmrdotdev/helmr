package computer

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/jackc/pgx/v5"
)

func TestCapacityNotificationsSeparateSavesAndExecution(t *testing.T) {
	f := newFixture(t)
	save := f.requestedSave(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listener, err := pgx.ConnectConfig(ctx, f.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(ctx)
	if _, err := listener.Exec(ctx, "LISTEN helmr_changes"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		notify   func(pgx.Tx) error
		capacity bool
		rollback bool
	}{
		{name: "save", notify: func(tx pgx.Tx) error { return notifySaveChanged(ctx, tx, f.env, save) }},
		{name: "execution", capacity: true, notify: func(tx pgx.Tx) error { return notifyComputerChanged(ctx, tx, f.env, f.computer) }},
		{name: "rolled back execution", rollback: true, notify: func(tx pgx.Tx) error { return notifyComputerChanged(ctx, tx, f.env, f.computer) }},
		{name: "released slot", capacity: true, notify: func(tx pgx.Tx) error { return FenceLease(ctx, tx, f.env, f.computer, 1, "test physical absence") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.notify(tx); err != nil {
				tx.Rollback(ctx)
				t.Fatal(err)
			}
			if tt.rollback {
				err = tx.Rollback(ctx)
			} else {
				err = tx.Commit(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			// A later committed marker on the same channel follows all hints
			// above, so absence is checked without a guessed no-event sleep.
			if err := db.RunTx(ctx, f.pool, func(tx pgx.Tx) error { return dbnotify.Notify(ctx, tx, "test-marker") }); err != nil {
				t.Fatal(err)
			}
			found := false
			for {
				event, err := listener.WaitForNotification(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if event.Payload == "test-marker" {
					break
				}
				if event.Payload == "capacity" {
					found = true
				}
			}
			if found != tt.capacity {
				t.Fatalf("capacity hint=%v want %v", found, tt.capacity)
			}
		})
	}
}
