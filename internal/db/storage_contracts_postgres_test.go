package db_test

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSchemaCanonicalContentAndRequestDigests(t *testing.T) {
	f := agenttest.New(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	turn := schemaTurn(t, f, tx)
	for _, value := range []string{"", "digest", "sha256:abc", "sha256:" + strings.Repeat("A", 64), "sha512:" + strings.Repeat("a", 64)} {
		rejectSchemaRow(t, tx, "23514", `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,1)`, value)
	}
	for _, size := range []int{0, 1, 31, 33} {
		rejectSchemaRow(t, tx, "23514", `UPDATE turns SET request_digest=$3 WHERE environment_id=$1 AND id=$2`, f.Environment, turn, make([]byte, size))
	}
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,1)`, dbtest.Digest("content"))
	for _, value := range []string{`null`, `[]`, `1`, `"text"`, `true`, `{"a":1}`} {
		dbtest.MustExec(t, t.Context(), tx, `UPDATE turns SET input=$3::bytea WHERE environment_id=$1 AND id=$2`, f.Environment, turn, value)
	}
	// Event membership binds both the Session and Turn, not a free-form subject.
	rejectSchemaRow(t, tx, "23503", `INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data) VALUES($1,$2,1,'00000000-0000-7000-8000-000000000001','queued','{}')`, f.Environment, f.Session)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data) VALUES($1,$2,1,$3,'queued','{}')`, f.Environment, f.Session, turn)
}
