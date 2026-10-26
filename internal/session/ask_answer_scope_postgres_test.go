package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAskAnswerAPIKeyMustBelongToEnvironment(t *testing.T) {
	f, _, ask, _ := askFixture(t)
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) SELECT $2,org_id,project_id,'other-answer','Other','#123456',max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at FROM environments WHERE id=$1`, f.env, other)
	ownKey, otherKey := uuid.NewV7(), uuid.NewV7()
	for _, pair := range []struct{ env, key uuid.UUID }{{f.env, ownKey}, {other, otherKey}} {
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash) SELECT $2,org_id,project_id,id,'answer','answer',$3 FROM environments WHERE id=$1`, pair.env, pair.key, []byte(pair.key.String()))
	}
	statement := `UPDATE turn_asks SET status='answered',answered_at=clock_timestamp(),answer_id='reply',answer_digest=decode(repeat('01',32),'hex'),answer='null',answered_by_api_key_id=$2 WHERE id=$1`
	_, err := f.pool.Exec(t.Context(), statement, ask, otherKey)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23503" {
		t.Fatalf("cross-environment answer: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, statement, ask, ownKey)
	// Retained answers continue to pin the answering credential after revocation.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE id=$1`, ownKey)
	_, err = f.pool.Exec(t.Context(), `DELETE FROM api_keys WHERE id=$1`, ownKey)
	if !errors.As(err, &pgerr) || pgerr.Code != "23503" {
		t.Fatalf("deleted historical answerer: %v", err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='answered' AND answered_by_api_key_id=$2 FROM turn_asks WHERE id=$1`, ask, ownKey).Scan(&retained); err != nil || !retained {
		t.Fatal(retained, err)
	}
}
