package db

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
)

func TestTokenTerminalQueriesPublishExactlyOneReconciliationIntent(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)

	t.Run("complete and replay", func(t *testing.T) {
		tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
		work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
		var workspaceID uuid.UUID
		if err := fixture.pool.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, work.runID).Scan(&workspaceID); err != nil {
			t.Fatal(err)
		}
		waitID := uuid.Must(uuid.NewV7())
		insertTokenWaitFixture(t, ctx, fixture, waitID, work.runID, workspaceID, tokenID, work.leaseID, 1)

		params := tokenCompletionParams(fixture, tokenID, "sha256:first", `{"approved":true}`)
		completed, err := fixture.queries.CompleteToken(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if completed.State != TokenStateCompleted || !completed.ReconciliationEnqueued ||
			completed.AlreadyCompleted || completed.CompletionConflict {
			t.Fatalf("first completion = %+v", completed)
		}
		assertTokenReconciliationIntent(t, ctx, fixture, tokenID, 1)
		var condition WaitState
		if err := fixture.pool.QueryRow(ctx, `SELECT condition_state FROM run_waits WHERE id = $1`, waitID).Scan(&condition); err != nil {
			t.Fatal(err)
		}
		if condition != WaitStatePending {
			t.Fatalf("Token transaction changed Run Wait condition to %s", condition)
		}

		replay, err := fixture.queries.CompleteToken(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if !replay.AlreadyCompleted || replay.CompletionConflict || replay.ReconciliationEnqueued {
			t.Fatalf("matching replay = %+v", replay)
		}
		conflictParams := tokenCompletionParams(fixture, tokenID, "sha256:different", `{"approved":false}`)
		conflict, err := fixture.queries.CompleteToken(ctx, conflictParams)
		if err != nil {
			t.Fatal(err)
		}
		if conflict.AlreadyCompleted || !conflict.CompletionConflict || conflict.ReconciliationEnqueued {
			t.Fatalf("conflicting replay = %+v", conflict)
		}
		assertTokenReconciliationIntent(t, ctx, fixture, tokenID, 1)
	})

	t.Run("cancel redelivery", func(t *testing.T) {
		tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
		cancelled, err := fixture.queries.CancelToken(ctx, tokenCancellationParams(fixture, tokenID))
		if err != nil {
			t.Fatal(err)
		}
		if cancelled.State != TokenStateCancelled || !cancelled.ReconciliationEnqueued {
			t.Fatalf("first cancellation = %+v", cancelled)
		}
		replay, err := fixture.queries.CancelToken(ctx, tokenCancellationParams(fixture, tokenID))
		if err != nil {
			t.Fatal(err)
		}
		if !replay.AlreadyCancelled || replay.ReconciliationEnqueued {
			t.Fatalf("cancellation replay = %+v", replay)
		}
		assertTokenReconciliationIntent(t, ctx, fixture, tokenID, 1)
	})

	t.Run("expiry redelivery", func(t *testing.T) {
		expiredAt := time.Now().Add(-time.Minute)
		tokenID := createTokenTerminalTestToken(t, ctx, fixture, expiredAt)
		publicAccessTokenID := uuid.Must(uuid.NewV7())
		mustRunLeaseExec(t, ctx, fixture.pool, `
			INSERT INTO public_access_tokens (
			    id, public_id, token_id, token_hash, credential_key_id,
			    created_at, updated_at, expires_at
			) VALUES (
			    $1, $2, $3, $4, 'test-key',
			    $5::timestamptz - interval '1 hour',
			    $5::timestamptz - interval '1 hour',
			    $5::timestamptz
			)
		`, publicAccessTokenID, runLeasePublicID(t, publicid.PublicAccessToken),
			tokenID, bytes.Repeat([]byte{2}, 32), expiredAt)
		expired, err := fixture.queries.ExpireDueTokens(ctx, ExpireDueTokensParams{
			OutboxMessageIds: pgvalue.NewUUIDv7Batch(100),
			LimitCount:       100,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(expired) != 1 || pgvalue.MustUUIDValue(expired[0].ID) != tokenID || expired[0].State != TokenStateExpired {
			t.Fatalf("first expiry = %+v", expired)
		}
		expired, err = fixture.queries.ExpireDueTokens(ctx, ExpireDueTokensParams{
			OutboxMessageIds: pgvalue.NewUUIDv7Batch(100),
			LimitCount:       100,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(expired) != 0 {
			t.Fatalf("expiry redelivery returned %+v", expired)
		}
		expiredCredentials, err := fixture.queries.ExpireDuePublicAccessTokens(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(expiredCredentials) != 1 ||
			pgvalue.MustUUIDValue(expiredCredentials[0].ID) != publicAccessTokenID ||
			expiredCredentials[0].State != PublicAccessTokenStateExpired {
			t.Fatalf("first credential expiry = %+v", expiredCredentials)
		}
		expiredCredentials, err = fixture.queries.ExpireDuePublicAccessTokens(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(expiredCredentials) != 0 {
			t.Fatalf("credential expiry redelivery returned %+v", expiredCredentials)
		}
		assertTokenReconciliationIntent(t, ctx, fixture, tokenID, 1)
	})
}

func TestTokenCompletionRollsBackWhenReconciliationIntentFails(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	mustRunLeaseExec(t, ctx, fixture.pool, `
		CREATE FUNCTION reject_token_reconciliation_intent() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.topic = 'token.reconcile' THEN
				RAISE EXCEPTION 'injected token reconciliation failure';
			END IF;
			RETURN NEW;
		END
		$$
	`)
	mustRunLeaseExec(t, ctx, fixture.pool, `
		CREATE TRIGGER reject_token_reconciliation_intent
		BEFORE INSERT ON outbox_messages
		FOR EACH ROW EXECUTE FUNCTION reject_token_reconciliation_intent()
	`)

	if _, err := fixture.queries.CompleteToken(ctx, tokenCompletionParams(fixture, tokenID, "sha256:rollback", `null`)); err == nil {
		t.Fatal("completion succeeded despite injected outbox failure")
	}
	var state TokenState
	var completionFingerprint []byte
	if err := fixture.pool.QueryRow(ctx, `
		SELECT state, completion_fingerprint FROM tokens WHERE id = $1
	`, tokenID).Scan(&state, &completionFingerprint); err != nil {
		t.Fatal(err)
	}
	if state != TokenStatePending || len(completionFingerprint) != 0 {
		t.Fatalf("rolled back Token = state %s fingerprint %x", state, completionFingerprint)
	}
	assertTokenReconciliationIntent(t, ctx, fixture, tokenID, 0)
}

func createTokenTerminalTestToken(t *testing.T, ctx context.Context, fixture runLeaseClaimFixture, timeoutAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	insertExpiry := timeoutAt
	if !insertExpiry.After(time.Now()) {
		insertExpiry = time.Now().Add(time.Hour)
	}
	row, err := fixture.queries.CreateToken(ctx, CreateTokenParams{
		ID: pgvalue.UUID(id), PublicID: runLeasePublicID(t, publicid.Token),
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ExpiresAt: pgvalue.Timestamptz(insertExpiry),
		CallbackKeyID: "test-key", CallbackSecretFingerprint: bytes.Repeat([]byte{1}, 32),
		Metadata: []byte(`{}`), Tags: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(row.ID) != id {
		t.Fatalf("created Token ID = %s, want %s", pgvalue.MustUUIDValue(row.ID), id)
	}
	if insertExpiry != timeoutAt {
		mustRunLeaseExec(t, ctx, fixture.pool, `
			UPDATE tokens
			   SET created_at = $2::timestamptz - interval '1 hour',
			       updated_at = $2::timestamptz - interval '1 hour',
			       expires_at = $2::timestamptz
			 WHERE id = $1
		`, id, timeoutAt)
	}
	return id
}

func tokenCompletionParams(fixture runLeaseClaimFixture, tokenID uuid.UUID, fingerprint string, data string) CompleteTokenParams {
	fingerprintBytes := []byte(strings.TrimPrefix(fingerprint, "sha256:"))
	if len(fingerprintBytes) < 32 {
		fingerprintBytes = append(fingerprintBytes, make([]byte, 32-len(fingerprintBytes))...)
	}
	return CompleteTokenParams{
		CompletionFingerprint: fingerprintBytes[:32], OrgID: pgvalue.UUID(fixture.orgID),
		ProjectID: pgvalue.UUID(fixture.projectID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		ID: pgvalue.UUID(tokenID), Result: []byte(data),
		OutboxMessageID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	}
}

func tokenCancellationParams(fixture runLeaseClaimFixture, tokenID uuid.UUID) CancelTokenParams {
	return CancelTokenParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ID: pgvalue.UUID(tokenID),
		OutboxMessageID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	}
}

func assertTokenReconciliationIntent(t *testing.T, ctx context.Context, fixture runLeaseClaimFixture, tokenID uuid.UUID, want int) {
	t.Helper()
	var count int
	var payloadMatches bool
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*)::integer,
		       coalesce(bool_and(payload = jsonb_build_object(
		           'environmentId', $1::uuid::text,
		           'tokenId', $2::uuid::text
		       )), true)
		  FROM outbox_messages
		 WHERE topic = 'token.reconcile'
		   AND partition_key = $2::uuid::text
	`, fixture.environmentID, tokenID).Scan(&count, &payloadMatches); err != nil {
		t.Fatal(err)
	}
	if count != want || !payloadMatches {
		t.Fatalf("Token reconciliation intents = count %d payload_matches %v, want %d/true", count, payloadMatches, want)
	}
}
