package slack

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"
)

func TestSlackHumanAdmissionLocksMemberBeforeLink(t *testing.T) {
	f := newStatusFixture(t)
	link := f.linkActor(t, f.User, "human")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	retirement, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer retirement.Rollback(t.Context())
	var member uuid.UUID
	if err = retirement.QueryRow(ctx, `SELECT member_id FROM slack_user_links WHERE id=$1`, link).Scan(&member); err != nil {
		t.Fatal(err)
	}
	if _, err = retirement.Exec(ctx, `SELECT id FROM org_members WHERE id=$1 FOR UPDATE`, member); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err = retirement.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		tx, err := f.Pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer tx.Rollback(ctx)
		_, denial, err := linkedHuman(ctx, tx, "team", "human", f.installation, f.Environment, f.Agent, time.Now())
		if err == nil && denial != "permission_denied" {
			err = errors.New("removed member admitted")
		}
		done <- err
	}()
	for {
		var waiting bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("admission did not wait %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if _, err = retirement.Exec(ctx, `SELECT id FROM slack_user_links WHERE id=$1 FOR UPDATE NOWAIT`, link); err != nil {
		t.Fatalf("link/member lock inversion: %v", err)
	}
	if _, err = retirement.Exec(ctx, `UPDATE org_members SET removed_at=clock_timestamp(),is_owner=false WHERE id=$1`, member); err != nil {
		t.Fatal(err)
	}
	if err = retirement.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
