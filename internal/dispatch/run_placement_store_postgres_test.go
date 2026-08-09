package dispatch

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunPlacementStorePagesOrganizationsAndScopes(t *testing.T) {
	ctx := context.Background()
	pool := newDispatchIntegrationDB(t, ctx)
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `
CREATE TEMP TABLE runs (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL,
    environment_id UUID NOT NULL,
    queue_name TEXT NOT NULL,
    concurrency_key TEXT,
    queue_score_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL,
    state_version BIGINT NOT NULL,
    current_run_lease_id UUID,
    first_lease_at TIMESTAMPTZ,
    queued_expires_at TIMESTAMPTZ
);
CREATE INDEX runs_dispatch_fair_idx
    ON runs (
        (get_byte(uuid_send(org_id), 15) & 63),
        org_id,
        environment_id,
        queue_name,
        (coalesce(concurrency_key, '')),
        queue_score_at,
        id
    )
    INCLUDE (state_version, first_lease_at, queued_expires_at)
    WHERE status = 'queued' AND current_run_lease_id IS NULL;
INSERT INTO runs (
    id, org_id, environment_id, queue_name, concurrency_key, queue_score_at,
    status, state_version
)
SELECT md5('run:' || organization_number::text || ':' || scope_number::text || ':' || run_number::text)::uuid,
       md5('organization:' || organization_number::text)::uuid,
       md5('environment:' || organization_number::text)::uuid,
       'queue-' || scope_number::text,
       CASE WHEN scope_number % 2 = 0 THEN NULL ELSE 'key-' || scope_number::text END,
       timestamptz '2026-01-01 00:00:00+00' + run_number * interval '1 millisecond',
       'queued',
       1
  FROM generate_series(0, 39) AS organization_number
 CROSS JOIN generate_series(0, 4) AS scope_number
 CROSS JOIN generate_series(0, 1) AS run_number;
ANALYZE runs`); err != nil {
		t.Fatal(err)
	}

	store, err := NewRunPlacementStore(connection)
	if err != nil {
		t.Fatal(err)
	}
	var lane int16
	var organizationCount int
	if err := connection.QueryRow(ctx, `
SELECT (get_byte(uuid_send(org_id), 15) & 63)::smallint AS lane,
       count(DISTINCT org_id)::integer
  FROM runs
 GROUP BY lane
 ORDER BY count(DISTINCT org_id) DESC, lane
 LIMIT 1`).Scan(&lane, &organizationCount); err != nil {
		t.Fatal(err)
	}
	organizations, err := store.ListOrganizations(ctx, lane, pgtype.UUID{}, int32(organizationCount+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(organizations) != organizationCount {
		t.Fatalf("first organization page = %d, want %d", len(organizations), organizationCount)
	}
	tail, err := store.ListOrganizations(ctx, lane, organizations[0], int32(organizationCount+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != organizationCount-1 {
		t.Fatalf("organization tail = %d, want %d", len(tail), organizationCount-1)
	}

	selected := organizations[:2]
	rows, err := store.ListScopes(ctx, runPlacementScopeParams{
		organizations: selected,
		after:         make([]runPlacementScopeCursor, len(selected)),
		limit:         3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("scope page = %d, want 6", len(rows))
	}
	for index, row := range rows {
		wantOrdinal := int64(index%2 + 1)
		if row.organizationOrdinal != wantOrdinal {
			t.Fatalf("scope %d organization ordinal = %d, want %d", index, row.organizationOrdinal, wantOrdinal)
		}
	}
	after := make([]runPlacementScopeCursor, len(selected))
	for _, row := range rows {
		index := int(row.organizationOrdinal - 1)
		after[index] = runPlacementScopeCursor{
			environmentID: row.scope.environmentID,
			queueName:     row.scope.queueName, concurrencyKey: row.scope.concurrencyKey, set: true,
		}
	}
	tailScopes, err := store.ListScopes(ctx, runPlacementScopeParams{
		organizations: selected,
		after:         after,
		limit:         3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tailScopes) != 4 {
		t.Fatalf("scope tail = %d, want 4", len(tailScopes))
	}
}
