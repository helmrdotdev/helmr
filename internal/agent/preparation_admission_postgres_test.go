package agent

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func TestPreparationAttachmentSharesAdmissionRollback(t *testing.T) {
	f := newPreparationFixture(t)
	computer := uuid.NewV7()
	rejected := errors.New("initial Turn rejected")
	var attempt uuid.UUID
	err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO computers(environment_id,id,preparation_spec_id,preparation_deadline_at) VALUES($1,$2,$3,clock_timestamp()+interval '10 minutes')`, f.env, computer, f.deployment); err != nil {
			return err
		}
		p, err := attachComputerPreparation(t.Context(), tx, f.env, computer)
		if err != nil {
			return err
		}
		attempt = p.ID
		return rejected
	})
	if !errors.Is(err, rejected) || attempt == uuid.Nil() {
		t.Fatalf("admission: %v", err)
	}
	var leaked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computers WHERE environment_id=$1 AND id=$2) OR EXISTS(SELECT 1 FROM computer_preparations WHERE environment_id=$1 AND id=$3)`, f.env, computer, attempt).Scan(&leaked); err != nil || leaked {
		t.Fatalf("rollback leaked demand or attempt: %v, %v", leaked, err)
	}
}

func TestPreparationImageSharesAdmissionRollback(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	computer := uuid.NewV7()
	rejected := errors.New("initial Turn rejected")
	var image uuid.UUID
	err := db.RunTx(t.Context(), p.f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO computers(environment_id,id,preparation_spec_id,preparation_deadline_at) VALUES($1,$2,$3,clock_timestamp()+interval '10 minutes')`, p.f.env, computer, p.f.deployment); err != nil {
			return err
		}
		selected, err := pinComputerImage(t.Context(), tx, p.f.env, computer)
		if err != nil {
			return err
		}
		image = selected.ID
		return rejected
	})
	if !errors.Is(err, rejected) || image == uuid.Nil() {
		t.Fatalf("admission: %v", err)
	}
	var leaked bool
	if err := p.f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computers WHERE environment_id=$1 AND id=$2)`, p.f.env, computer).Scan(&leaked); err != nil || leaked {
		t.Fatalf("rollback leaked Computer or root pin: %v, %v", leaked, err)
	}
	// The shared image survives another admission's rollback and remains usable.
	fresh := p.f.waiter(t)
	selected, err := PinComputerImage(t.Context(), p.f.pool, p.f.env, fresh)
	if err != nil || selected.ID != image {
		t.Fatalf("shared image lost: %v, %v", selected, err)
	}
}

func TestPreparationUnattachedDemandWaitsForPhysicalExclusion(t *testing.T) {
	f := newPreparationFixture(t)
	originalComputer := f.waiter(t)
	original := f.attach(t, originalComputer)
	executor := f.claim(t, original)
	if err := FailPreparation(t.Context(), f.pool, *f.host(), executor, "prepare_failed"); err != nil {
		t.Fatal(err)
	}
	fresh := f.waiter(t)
	var deadline time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT preparation_deadline_at FROM computers WHERE environment_id=$1 AND id=$2`, f.env, fresh).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	reconcile := func() {
		t.Helper()
		if _, _, err := reconcilePreparationLifecycle(t.Context(), f.pool, preparationLifecyclePosition{}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	var attached *uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT preparation_id FROM computers WHERE environment_id=$1 AND id=$2`, f.env, fresh).Scan(&attached); err != nil || attached != nil {
		t.Fatalf("unfenced executor admitted replacement: %v %v", attached, err)
	}
	if err := ObservePreparationStopped(t.Context(), f.pool, *f.host(), executor.Identity()); err != nil {
		t.Fatal(err)
	}
	reconcile()
	var after time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT preparation_id,preparation_deadline_at FROM computers WHERE environment_id=$1 AND id=$2`, f.env, fresh).Scan(&attached, &after); err != nil || attached == nil || *attached == original.ID || !after.Equal(deadline) {
		t.Fatalf("fresh demand not attached under original deadline: %v %v %v", attached, after, err)
	}
	var retained uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT preparation_id FROM computers WHERE environment_id=$1 AND id=$2`, f.env, originalComputer).Scan(&retained); err != nil || retained != original.ID {
		t.Fatalf("failed chain was retried: %v %v", retained, err)
	}
	reconcile()
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_preparations WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate preparation: %d %v", count, err)
	}
}

func TestPreparationUnattachedDemandExpiresWithoutAttempt(t *testing.T) {
	f := newPreparationFixture(t)
	fresh := f.waiter(t)
	if _, err := f.pool.Exec(t.Context(), `UPDATE computers SET preparation_deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, fresh); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconcilePreparationLifecycle(t.Context(), f.pool, preparationLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var settled bool
	if err := f.pool.QueryRow(t.Context(), `SELECT preparation_failed_at IS NOT NULL AND preparation_id IS NULL AND NOT EXISTS(SELECT 1 FROM computer_preparations WHERE environment_id=$1) FROM computers WHERE environment_id=$1 AND id=$2`, f.env, fresh).Scan(&settled); err != nil || !settled {
		t.Fatalf("unattached demand not settled: %v %v", settled, err)
	}
}

func TestPreparationFreshDemandDoesNotPreemptRotationSuccessor(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	originalComputer := p.f.waiter(t)
	original := p.f.attach(t, originalComputer)
	// Existing demand has less time left than the later fresh Computer. Joining
	// a chain does not promise a fresh execution window or extend its deadline.
	var originalDeadline time.Time
	if err := p.f.pool.QueryRow(t.Context(), `UPDATE computers SET preparation_deadline_at=statement_timestamp()+interval '1 minute' WHERE environment_id=$1 AND preparation_id=$2 RETURNING preparation_deadline_at`, p.f.env, original.ID).Scan(&originalDeadline); err != nil {
		t.Fatal(err)
	}
	root := p.capture(t, key)
	if _, err := p.f.secrets.Rotate(t.Context(), p.f.env, p.f.secretID, []byte("v2"), "rotate"); err != nil {
		t.Fatal(err)
	}
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	if err := ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
		t.Fatal(err)
	}
	// Sort fresh demand before every attached waiter in the actual scan page.
	fresh := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	if _, err := p.f.pool.Exec(t.Context(), `INSERT INTO computers(environment_id,id,preparation_spec_id,preparation_deadline_at) VALUES($1,$2,$3,clock_timestamp()+interval '10 minutes')`, p.f.env, fresh, p.f.deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := AttachComputerPreparation(t.Context(), p.f.pool, p.f.env, fresh); !errors.Is(err, ErrNotReady) {
		t.Fatalf("fresh demand preempted pending successor: %v", err)
	}
	for range 2 {
		if _, _, err := reconcilePreparationLifecycle(t.Context(), p.f.pool, preparationLifecyclePosition{}); err != nil {
			t.Fatal(err)
		}
	}
	var successor, predecessor, retained uuid.UUID
	var count int
	if err := p.f.pool.QueryRow(t.Context(), `SELECT c.preparation_id,p.successor_of,(SELECT count(*) FROM computer_preparations WHERE environment_id=$1) FROM computers c JOIN computer_preparations p ON (p.environment_id,p.id)=(c.environment_id,c.preparation_id) WHERE c.environment_id=$1 AND c.id=$2`, p.f.env, fresh).Scan(&successor, &predecessor, &count); err != nil || predecessor != original.ID || count != 2 {
		t.Fatalf("fresh demand did not join the single successor: %v %v %d %v", successor, predecessor, count, err)
	}
	if err := p.f.pool.QueryRow(t.Context(), `SELECT preparation_id FROM computers WHERE environment_id=$1 AND id=$2`, p.f.env, originalComputer).Scan(&retained); err != nil || retained != original.ID {
		t.Fatalf("original chain changed: %v %v", retained, err)
	}
	var attemptDeadline, freshDeadline time.Time
	if err := p.f.pool.QueryRow(t.Context(), `SELECT p.deadline_at,c.preparation_deadline_at FROM computer_preparations p JOIN computers c ON (c.environment_id,c.preparation_id)=(p.environment_id,p.id) WHERE c.environment_id=$1 AND c.id=$2`, p.f.env, fresh).Scan(&attemptDeadline, &freshDeadline); err != nil || !attemptDeadline.Equal(originalDeadline) || !freshDeadline.After(attemptDeadline) {
		t.Fatalf("join changed deadlines: %v %v %v", attemptDeadline, freshDeadline, err)
	}
	if _, err := p.f.pool.Exec(t.Context(), `UPDATE computer_preparations SET deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, p.f.env, successor); err != nil {
		t.Fatal(err)
	}
	if err := expirePreparation(t.Context(), p.f.pool, p.f.env, successor); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconcilePreparationLifecycle(t.Context(), p.f.pool, preparationLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var unchanged bool
	if err := p.f.pool.QueryRow(t.Context(), `SELECT c.preparation_id=$3 AND c.preparation_deadline_at=$4 AND (SELECT count(*) FROM computer_preparations WHERE environment_id=$1)=2 FROM computers c WHERE environment_id=$1 AND id=$2`, p.f.env, fresh, successor, freshDeadline).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("joined expiry created another attempt or moved deadline: %v %v", unchanged, err)
	}

}

func TestPreparationUnattachedDemandInRetiredEnvironmentIsNotPolled(t *testing.T) {
	f := newPreparationFixture(t)
	fresh := f.waiter(t)
	if _, err := f.pool.Exec(t.Context(), `UPDATE environments SET retired_at=clock_timestamp() WHERE id=$1`, f.env); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconcilePreparationLifecycle(t.Context(), f.pool, preparationLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var pending bool
	if err := f.pool.QueryRow(t.Context(), `SELECT preparation_id IS NULL AND preparation_failed_at IS NULL FROM computers WHERE environment_id=$1 AND id=$2`, f.env, fresh).Scan(&pending); err != nil || !pending {
		t.Fatalf("retired environment created work: %v %v", pending, err)
	}
}
