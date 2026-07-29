package idempotency

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresClaimUniqueSlotAndStaleCompletionFence(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	environmentID := seedClaimEnvironment(t, database.Pool)
	request, err := NewSecretCreateRequest(environmentID, "API_TOKEN", "create-1")
	if err != nil {
		t.Fatal(err)
	}

	results := make(chan Result, 2)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			tx, err := database.Pool.Begin(context.Background())
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(context.Background())
			claims, err := TransactionFor(tx)
			if err != nil {
				errs <- err
				return
			}
			result, err := claims.Acquire(context.Background(), request)
			if err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(context.Background()); err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(results)
	var first db.IdempotencyClaim
	newCount := 0
	for result := range results {
		if first.ID.Valid && result.Claim.ID != first.ID {
			t.Fatalf("same live slot produced claims %v and %v", first.ID, result.Claim.ID)
		}
		first = result.Claim
		if result.New {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("new claim count = %d", newCount)
	}

	if _, err := database.Pool.Exec(t.Context(), `
		UPDATE idempotency_claims
		   SET accepted_at = statement_timestamp() - interval '31 days',
		       expires_at = statement_timestamp() - interval '1 day'
		 WHERE id = $1
	`, first.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	claims, err := TransactionFor(tx)
	if err != nil {
		t.Fatal(err)
	}
	rebound, err := claims.Acquire(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !rebound.New || rebound.Claim.ID == first.ID {
		t.Fatalf("rebound claim = %+v", rebound)
	}
	if _, err := claims.Complete(
		t.Context(),
		first,
		[]byte(`{"secretId":"stale"}`),
	); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale completion error = %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresClaimSlotIsScopedByEnvironment(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	firstEnvironmentID := seedClaimEnvironment(t, database.Pool)
	secondEnvironmentID := seedClaimEnvironment(t, database.Pool)

	acquire := func(environmentID uuid.UUID) db.IdempotencyClaim {
		t.Helper()
		request, err := NewSecretCreateRequest(environmentID, "API_TOKEN", "create-1")
		if err != nil {
			t.Fatal(err)
		}
		tx, err := database.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		claims, err := TransactionFor(tx)
		if err != nil {
			t.Fatal(err)
		}
		result, err := claims.Acquire(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		return result.Claim
	}

	first := acquire(firstEnvironmentID)
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	_, err = db.New(tx).LockLiveIdempotencyClaim(
		t.Context(),
		db.LockLiveIdempotencyClaimParams{
			EnvironmentID: pgvalue.UUID(secondEnvironmentID),
			Operation:     first.Operation,
			SlotHash:      first.SlotHash,
		},
	)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-environment claim lookup error = %v", err)
	}

	second := acquire(secondEnvironmentID)
	if first.ID == second.ID {
		t.Fatalf("different environments shared claim %v", first.ID)
	}
}

func seedClaimEnvironment(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	orgID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	regionID := "claim-" + environmentID.String()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Claims', $2)
	`, orgID, "claims-"+orgID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'test', $1, 'Claims')
	`, regionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, 'Claims')
	`,
		projectID,
		orgID,
		regionID,
		"claims-"+projectID.String(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, 'production', 'Production', '#000000')
	`, environmentID, orgID, projectID); err != nil {
		t.Fatal(err)
	}
	return environmentID
}
