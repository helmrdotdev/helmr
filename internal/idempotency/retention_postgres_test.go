package idempotency

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
)

func TestReceiptCollectionSkipsLockedPendingAndRetainedClaims(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	environmentID := seedClaimEnvironment(t, database.Pool)
	ids := make([]uuid.UUID, 5)
	for i := range ids {
		ids[i] = uuid.NewV7()
		dbtest.MustExec(t, t.Context(), database.Pool, `
INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,
 status,receipt,accepted_at,completed_at,receipt_expires_at)
VALUES($1,$2,'secret.create',$3,$3,'completed','{}',now(),now(),now()-interval '1 day')`,
			ids[i], environmentID, dbtest.Hash(ids[i].String()))
	}
	dbtest.MustExec(t, t.Context(), database.Pool, `UPDATE idempotency_claims
 SET status='pending',completed_at=NULL,receipt=NULL WHERE id=$1`, ids[2])
	dbtest.MustExec(t, t.Context(), database.Pool, `UPDATE idempotency_claims
 SET receipt_expires_at=now()+interval '1 day' WHERE id=$1`, ids[3])
	dbtest.MustExec(t, t.Context(), database.Pool, `UPDATE idempotency_claims
 SET receipt_expires_at=NULL WHERE id=$1`, ids[4])
	locked, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), locked, `SELECT id FROM idempotency_claims WHERE id=$1 FOR UPDATE`, ids[0])
	queries := db.New(database.Pool)
	if count, err := queries.PruneExpiredIdempotencyReceipts(t.Context(), 1); err != nil || count != 1 {
		t.Fatalf("collect unlocked receipt = %d, %v", count, err)
	}
	if count, err := queries.PruneExpiredIdempotencyReceipts(t.Context(), 100); err != nil || count != 0 {
		t.Fatalf("collect retained receipts = %d, %v", count, err)
	}
	if err := locked.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if count, err := queries.PruneExpiredIdempotencyReceipts(t.Context(), 100); err != nil || count != 1 {
		t.Fatalf("collect released receipt = %d, %v", count, err)
	}
	var claims, bodies int
	if err := database.Pool.QueryRow(t.Context(), `SELECT count(*),count(receipt)
 FROM idempotency_claims WHERE environment_id=$1`, environmentID).Scan(&claims, &bodies); err != nil {
		t.Fatal(err)
	}
	if claims != 5 || bodies != 2 {
		t.Fatalf("retained identities=%d bodies=%d", claims, bodies)
	}
}
