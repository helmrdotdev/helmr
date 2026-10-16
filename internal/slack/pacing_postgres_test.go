package slack

import (
	"fmt"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

func TestDeliveryPacingSeparatesChannelsStatusAndFinalUpdates(t *testing.T) {
	f := newStatusFixture(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	reserve := func(channel, method string, final, want bool) {
		t.Helper()
		got, err := reserveDelivery(t.Context(), tx, f.installation, channel, method, final)
		if err != nil || got != want {
			t.Fatalf("reserve %s/%s final=%v: %v %v", channel, method, final, got, err)
		}
	}
	reserve("C1", "chat.postMessage", false, true)
	reserve("C1", "chat.postMessage", false, false)
	reserve("C2", "chat.postMessage", false, true)
	reserve("C1", "agents.sessions.setStatus", false, true)
	reserve("C1", "agents.sessions.setStatus", false, false)
	reserve("C1", "chat.update", true, true)
	reserve("C1", "chat.stopStream", true, true)
	if err := recordMethodBackoff(t.Context(), tx, f.installation, "chat.update", time.Minute); err != nil {
		t.Fatal(err)
	}
	reserve("C1", "chat.update", true, false)
	reserve("C2", "chat.update", true, false)
	reserve("C1", "chat.stopStream", true, true)
	if err := recordMethodBackoff(t.Context(), tx, f.installation, "conversations.info", time.Minute); err != nil {
		t.Fatal(err)
	}
	reserve("C1", "chat.stopStream", true, false)
}

func TestConcurrentDeliveryReservationsAndBackoffAreShared(t *testing.T) {
	f := newStatusFixture(t)
	const callers = 8
	results := make(chan bool, callers)
	failures := make(chan error, callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			<-start
			var reserved bool
			err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				// Delivery's existing installation gate orders provider backoff and reservation.
				if _, err := tx.Exec(t.Context(), `SELECT id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, f.installation); err != nil {
					return err
				}
				var err error
				reserved, err = reserveDelivery(t.Context(), tx, f.installation, "C1", "chat.postMessage", false)
				return err
			})
			results <- reserved
			failures <- err
		}()
	}
	close(start)
	winners := 0
	for range callers {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
		if <-results {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("same channel admitted %d concurrent reservations", winners)
	}
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SELECT id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, f.installation); err != nil {
			return err
		}
		if err := recordMethodBackoff(t.Context(), tx, f.installation, "chat.update", time.Hour); err != nil {
			return err
		}
		return recordMethodBackoff(t.Context(), tx, f.installation, "chat.update", time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		for _, channel := range []string{"C1", "C2"} {
			if ok, err := reserveDelivery(t.Context(), tx, f.installation, channel, "chat.update", true); err != nil || ok {
				return fmt.Errorf("method backoff did not cover %s: reserved=%v error=%v", channel, ok, err)
			}
		}
		var retained bool
		if err := tx.QueryRow(t.Context(), `SELECT retry_after>clock_timestamp()+interval '59 minutes' FROM slack_method_limits WHERE installation_id=$1 AND method='chat.update'`, f.installation).Scan(&retained); err != nil {
			return err
		}
		if !retained {
			return fmt.Errorf("shorter retry overwrote longer provider backoff")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
