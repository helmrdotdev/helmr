package db_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestWorkerEnrollmentTokenSelectsGroupAndRecordsUse(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	params := db.EnrollWorkerInstanceParams{
		TokenHash: make([]byte, 32), AllowsRun: true, AllowsBuild: true,
		WorkerPoolID: pgvalue.UUID(uuid.MustParse(dbtest.DefaultWorkerPoolID)), PoolName: "default",
		WorkerInstanceID: pgvalue.NewUUIDv7(), ResourceID: "enrollment-host",
		CurrentServiceID: pgvalue.NewUUIDv7(), CredentialID: pgvalue.NewUUIDv7(),
		KeyPrefix: uuid.NewString(), SecretHash: []byte("instance-secret"),
	}
	credential, err := q.EnrollWorkerInstance(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if credential.WorkerGroupID != dbtest.DefaultWorkerGroupID || !credential.AllowsRun || !credential.AllowsBuild {
		t.Fatalf("credential = %+v", credential)
	}
	var used bool
	if err := pool.QueryRow(ctx, `
		SELECT token.last_used_at IS NOT NULL
		  FROM worker_group_tokens AS token
		  JOIN worker_groups AS group_record ON group_record.token_id = token.id
		 WHERE group_record.id = $1
	`, dbtest.DefaultWorkerGroupID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("enrollment token use was not recorded")
	}
}

func TestWorkerEnrollmentRejectsUnknownTokenAndUnallowedRole(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	base := db.EnrollWorkerInstanceParams{
		TokenHash: make([]byte, 32), AllowsRun: true, AllowsBuild: true,
		WorkerPoolID: pgvalue.UUID(uuid.MustParse(dbtest.DefaultWorkerPoolID)), PoolName: "default",
		WorkerInstanceID: pgvalue.NewUUIDv7(), ResourceID: "unknown-token-host",
		CurrentServiceID: pgvalue.NewUUIDv7(), CredentialID: pgvalue.NewUUIDv7(),
		KeyPrefix: uuid.NewString(), SecretHash: []byte("instance-secret"),
	}
	unknown := base
	unknown.TokenHash = bytes.Repeat([]byte{1}, 32)
	if _, err := q.EnrollWorkerInstance(ctx, unknown); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown token error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE worker_groups SET allows_build = false WHERE id = $1`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}
	wrongRole := base
	wrongRole.WorkerInstanceID = pgvalue.NewUUIDv7()
	wrongRole.CredentialID = pgvalue.NewUUIDv7()
	wrongRole.ResourceID = "wrong-role-host"
	if _, err := q.EnrollWorkerInstance(ctx, wrongRole); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unallowed role error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE worker_groups SET allows_build = true WHERE id = $1`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.TransitionWorkerGroupState(ctx, db.TransitionWorkerGroupStateParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStatePaused), ExpectedClaimVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.TransitionWorkerGroupState(ctx, db.TransitionWorkerGroupStateParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID, TargetState: string(db.WorkerGroupStateDraining), ExpectedClaimVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}
	draining := base
	draining.WorkerInstanceID = pgvalue.NewUUIDv7()
	draining.CredentialID = pgvalue.NewUUIDv7()
	draining.ResourceID = "draining-host"
	if _, err := q.EnrollWorkerInstance(ctx, draining); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("draining group enrollment error = %v", err)
	}
}
