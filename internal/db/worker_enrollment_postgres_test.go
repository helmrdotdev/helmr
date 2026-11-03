package db_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestWorkerEnrollmentRegistersTheHostPublicKey(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.NewV7()
	params := enrollmentParams(workerID, "enrollment-host", newHostPublicKey(t))
	enrolled, err := q.EnrollWorkerHost(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if enrolled.WorkerGroupID != dbtest.DefaultWorkerGroupID || enrolled.ID != params.HostKeyID || !bytes.Equal(enrolled.PublicKey, params.PublicKey) {
		t.Fatalf("enrolled = %+v", enrolled)
	}
	active, err := q.GetActiveWorkerHostKey(ctx, pgvalue.UUID(workerID))
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != params.HostKeyID || !bytes.Equal(active.PublicKey, params.PublicKey) {
		t.Fatalf("active key = %+v, want %+v", active, params)
	}

	// Enrolling the registering host again replaces its key.
	again := enrollmentParams(workerID, "enrollment-host", newHostPublicKey(t))
	if _, err := q.EnrollWorkerHost(ctx, again); err != nil {
		t.Fatal(err)
	}
	active, err = q.GetActiveWorkerHostKey(ctx, pgvalue.UUID(workerID))
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != again.HostKeyID || !bytes.Equal(active.PublicKey, again.PublicKey) {
		t.Fatalf("active key after re-enrollment = %+v, want %+v", active, again)
	}
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM worker_host_keys WHERE id=$1`, params.HostKeyID).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("replaced key revoked = %v, %v", revoked, err)
	}
	if _, err := q.GetActiveWorkerHostKey(ctx, pgvalue.NewUUIDv7()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown host key error = %v, want pgx.ErrNoRows", err)
	}
}

func TestWorkerEnrollmentRejectsReusedAndMalformedPublicKeys(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	key := newHostPublicKey(t)
	first := enrollmentParams(uuid.NewV7(), "first-key-host", key)
	if _, err := q.EnrollWorkerHost(ctx, first); err != nil {
		t.Fatal(err)
	}
	for name, params := range map[string]db.EnrollWorkerHostParams{
		// A revoked key never registers again, so the same host re-enrolling
		// with it is refused as well.
		"same host":    enrollmentParams(uuid.NewV7(), "first-key-host", key),
		"another host": enrollmentParams(uuid.NewV7(), "second-key-host", key),
	} {
		t.Run(name, func(t *testing.T) {
			var pgErr *pgconn.PgError
			if _, err := q.EnrollWorkerHost(ctx, params); !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "worker_host_keys_public_key_key" {
				t.Fatalf("reused key error = %v", err)
			}
		})
	}
	var pgErr *pgconn.PgError
	if _, err := q.EnrollWorkerHost(ctx, enrollmentParams(uuid.NewV7(), "short-key-host", key[:31])); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("short key error = %v", err)
	}
}
