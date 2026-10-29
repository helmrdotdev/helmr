package db_test

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"testing"
	"uuid"
)

func TestAuthIdentityCreationDoesNotCorrelateOrOverwriteAccountByEmail(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	q := db.New(d.Pool)
	first, second := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name,primary_email) VALUES($1,'Original','original@example.test'),($2,'Other','other@example.test')`, first, second)
	source, err := q.CreateAuthIdentity(t.Context(), db.CreateAuthIdentityParams{ID: pgvalue.NewUUIDv7(), UserID: pgvalue.UUID(first), Provider: "github", Subject: "123", ObservedEmail: pgtype.Text{String: "other@example.test", Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.CreateAuthIdentity(t.Context(), db.CreateAuthIdentityParams{ID: pgvalue.NewUUIDv7(), UserID: pgvalue.UUID(second), Provider: "github", Subject: "123"}); err == nil {
		t.Fatal("same live subject transferred to another account")
	}
	found, err := q.GetAuthIdentity(t.Context(), db.GetAuthIdentityParams{Provider: "github", Subject: "123"})
	if err != nil || found.ID != source.ID || found.UserID != pgvalue.UUID(first) {
		t.Fatalf("source changed: %+v %v", found, err)
	}
	var unchanged bool
	if err = d.Pool.QueryRow(t.Context(), `SELECT display_name='Original' AND primary_email='original@example.test' FROM users WHERE id=$1`, first).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("provider email overwrote profile: %v %v", unchanged, err)
	}
}
