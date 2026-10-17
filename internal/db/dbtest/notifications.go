package dbtest

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ListenChanges returns all change topics committed since its previous call.
// A marker on a separate connection bounds each drain without a timing sleep.
func ListenChanges(t *testing.T, pool *pgxpool.Pool) func() map[string]bool {
	t.Helper()
	listener, err := pgx.ConnectConfig(t.Context(), pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(context.Background()) })
	if _, err = listener.Exec(t.Context(), "LISTEN helmr_changes"); err != nil {
		t.Fatal(err)
	}
	return func() map[string]bool {
		t.Helper()
		marker := uuid.NewV7().String()
		if _, err := pool.Exec(t.Context(), `SELECT pg_notify('helmr_changes',$1)`, marker); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		result := map[string]bool{}
		for {
			notification, err := listener.WaitForNotification(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if notification.Payload == marker {
				return result
			}
			result[notification.Payload] = true
		}
	}
}
