package org_test

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/org"
	"testing"
	"time"
	"uuid"
)

func TestOrganizationManagementRetainsOriginalHumanSource(t *testing.T) {
	for _, change := range []string{"logout", "credential", "proof", "membership", "suspension"} {
		t.Run(change, func(t *testing.T) {
			f := newMemberFixture(t)
			original := f.owner
			if _, err := org.SavePermissionGroup(t.Context(), f.pool, original, uuid.Nil(), "Before"); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "logout":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, original.Human.SessionID)
			case "credential":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE id=$1`, original.Human.IdentityID)
			case "proof":
				fresh := dbtest.BrowserPrincipal(t, f.pool, original.OrgID, original.UserID)
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE login_sessions SET identity_id=$2 WHERE id=$1`, original.Human.SessionID, fresh.IdentityID)
			case "membership":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE org_members SET removed_at=clock_timestamp(),is_owner=false WHERE id=$1`, original.MemberID)
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO org_members(id,user_id,org_id,is_owner) VALUES($1,$2,$3,true)`, uuid.NewV7(), original.UserID, original.OrgID)
			case "suspension":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE org_members SET suspended_at=clock_timestamp() WHERE id=$1`, original.MemberID)
			}
			if _, err := org.SavePermissionGroup(t.Context(), f.pool, original, uuid.Nil(), "After"); !errors.Is(err, org.ErrMemberManagementRequired) {
				t.Fatalf("old source admitted %v", err)
			}
			if _, err := org.RecoverOwnAccess(t.Context(), f.pool, original); err == nil {
				t.Fatal("stale source recovered owner access")
			}
			var count int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM permission_groups WHERE name='After'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("mutation committed %d %v", count, err)
			}
		})
	}
}

func TestHumanAdmissionDoesNotLockSessionBeforeMembership(t *testing.T) {
	f := newMemberFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	retirement, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer retirement.Rollback(t.Context())
	if _, err = retirement.Exec(ctx, `SELECT id FROM org_members WHERE id=$1 FOR UPDATE`, f.member.MemberID); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err = retirement.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer tx.Rollback(ctx)
		_, err = access.CurrentHuman(ctx, tx, f.member.Human)
		done <- err
	}()
	for {
		var waiting bool
		if err = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("admission did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	// Admission is waiting on the membership, so it must not already hold the
	// channel row that retirement needs after that membership.
	if _, err = retirement.Exec(ctx, `SELECT id FROM login_sessions WHERE id=$1 FOR UPDATE NOWAIT`, f.member.Human.SessionID); err != nil {
		t.Fatalf("session/member lock inversion: %v", err)
	}
	if _, err = retirement.Exec(ctx, `UPDATE org_members SET removed_at=clock_timestamp() WHERE id=$1`, f.member.MemberID); err != nil {
		t.Fatal(err)
	}
	if err = retirement.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, access.ErrDenied) {
		t.Fatalf("removed member admitted: %v", err)
	}
}
