package control

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCreateWithPublicIDRetriesPublicIDCollision(t *testing.T) {
	t.Parallel()
	var value string
	attempts := 0
	got, err := createWithPublicID(context.Background(), []publicIDSlot{{prefix: publicid.Run, value: &value}}, func() (string, error) {
		attempts++
		if value == "" {
			t.Fatal("public id was not generated before create")
		}
		if attempts == 1 {
			return "", &pgconn.PgError{Code: "23505", ConstraintName: "runs_public_id_key"}
		}
		return value, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if err := publicid.ValidateFor(publicid.Run, got); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWithPublicIDDoesNotRetryOtherUniqueViolation(t *testing.T) {
	t.Parallel()
	var value string
	attempts := 0
	_, err := createWithPublicID(context.Background(), []publicIDSlot{{prefix: publicid.Run, value: &value}}, func() (string, error) {
		attempts++
		return "", &pgconn.PgError{Code: "23505", ConstraintName: "runs_org_id_slug_key"}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "runs_org_id_slug_key" {
		t.Fatalf("error = %v, want original unique violation", err)
	}
}

func TestCreateWithPublicIDExhaustionDoesNotReturnUniqueViolation(t *testing.T) {
	t.Parallel()
	var value string
	_, err := createWithPublicID(context.Background(), []publicIDSlot{{prefix: publicid.Run, value: &value}}, func() (string, error) {
		return "", &pgconn.PgError{Code: "23505", ConstraintName: "runs_public_id_key"}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want non-pg exhausted error", err)
	}
}
