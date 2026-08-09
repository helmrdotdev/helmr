package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type dispatchLaneMeasurement struct {
	Scenario                string  `json:"scenario"`
	Rows                    int     `json:"rows"`
	Organizations           int     `json:"organizations"`
	Scopes                  int     `json:"scopes"`
	CandidateHeads          int     `json:"candidate_heads"`
	Statements              int     `json:"statements"`
	MinimumMillis           float64 `json:"minimum_millis"`
	MedianMillis            float64 `json:"median_millis"`
	P95Millis               float64 `json:"p95_millis"`
	MaximumMillis           float64 `json:"maximum_millis"`
	CandidateHeadsPerSecond float64 `json:"candidate_heads_per_second"`
}

func TestMeasureDispatchHierarchy(t *testing.T) {
	if os.Getenv(dispatchMeasurementEnabled) != "1" {
		t.Skip(dispatchMeasurementEnabled + "=1 is required")
	}

	ctx := context.Background()
	pool := newDispatchIntegrationDB(t, ctx)
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()

	createDispatchLaneMeasurementTable(t, ctx, connection.Conn())
	scenarios := []struct {
		name          string
		rows          int
		organizations int
		scopes        int
		skewed        bool
	}{
		{name: "lane_10000_organizations", rows: 1_000_000, organizations: 10_000, scopes: 100_000},
		{name: "lane_10000_organizations_skewed", rows: 1_000_000, organizations: 10_000, scopes: 100_000, skewed: true},
	}

	for _, scenario := range scenarios {
		if selected := os.Getenv("HELMR_MEASURE_DISPATCH_SCENARIO"); selected != "" && selected != scenario.name {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			seedDispatchLaneMeasurement(
				t, ctx, connection.Conn(), scenario.rows, scenario.organizations, scenario.scopes, scenario.skewed,
			)
			store, err := NewRunPlacementStore(connection)
			if err != nil {
				t.Fatal(err)
			}
			measurement := measureDispatchLaneCoverage(
				t, ctx, store, scenario.name, scenario.rows, scenario.organizations, scenario.scopes,
			)
			payload, err := json.Marshal(measurement)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("dispatch_lane_measurement=%s", payload)
			if measurement.MaximumMillis > 10_000 {
				t.Fatalf("dispatch lane coverage = %.3fms, want <= 10000ms", measurement.MaximumMillis)
			}
			if measurement.CandidateHeadsPerSecond < 1_200 {
				t.Fatalf("dispatch lane candidate heads = %.1f/s, want >= 1200/s", measurement.CandidateHeadsPerSecond)
			}
		})
	}
}

func createDispatchLaneMeasurementTable(t *testing.T, ctx context.Context, connection *pgx.Conn) {
	t.Helper()
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
    WHERE status = 'queued' AND current_run_lease_id IS NULL`); err != nil {
		t.Fatal(err)
	}
}

func seedDispatchLaneMeasurement(
	t *testing.T,
	ctx context.Context,
	connection *pgx.Conn,
	rows int,
	organizations int,
	scopes int,
	skewed bool,
) {
	t.Helper()
	if rows < 1 || organizations < 1 || scopes < organizations || scopes > rows {
		t.Fatalf("invalid lane measurement shape rows=%d organizations=%d scopes=%d", rows, organizations, scopes)
	}
	if _, err := connection.Exec(ctx, `TRUNCATE runs`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `
WITH generated AS (
    SELECT series AS row_number,
           series % $2::bigint AS scope_number
      FROM generate_series(0::bigint, $1::bigint - 1) AS series
), assigned AS (
    SELECT generated.*,
           CASE
             WHEN $4::boolean AND scope_number < ($2::bigint * 9 / 10)
               THEN 0::bigint
             WHEN $4::boolean
               THEN 1 + scope_number % ($3::bigint - 1)
             ELSE scope_number % $3::bigint
           END AS organization_number
      FROM generated
)
INSERT INTO runs (
    id, org_id, environment_id, queue_name, concurrency_key, queue_score_at,
    status, state_version, current_run_lease_id, first_lease_at, queued_expires_at
)
SELECT md5('run:' || row_number::text)::uuid,
       md5('organization:' || organization_number::text)::uuid,
       md5('environment:' || organization_number::text || ':' || ((scope_number / $3::bigint) % 4)::text)::uuid,
       'measure-' || lpad(scope_number::text, 6, '0'),
       CASE WHEN scope_number % 2 = 0 THEN NULL ELSE 'key-' || lpad(scope_number::text, 6, '0') END,
       timestamptz '2026-01-01 00:00:00+00' + row_number * interval '1 millisecond',
       'queued',
       1,
       NULL,
       NULL,
       NULL
  FROM assigned`, rows, scopes, organizations, skewed); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `VACUUM (ANALYZE) runs`); err != nil {
		t.Fatal(err)
	}
}

func measureDispatchLaneCoverage(
	t *testing.T,
	ctx context.Context,
	store *RunPlacementStore,
	name string,
	rows int,
	organizations int,
	scopes int,
) dispatchLaneMeasurement {
	t.Helper()
	const iterations = 5
	durations := make([]time.Duration, 0, iterations)
	candidateHeads := 0
	statements := 0
	for iteration := 0; iteration < iterations+1; iteration++ {
		started := time.Now()
		var measuredStatements int
		candidateHeads, measuredStatements = scanEveryDispatchLane(t, ctx, store)
		if candidateHeads != organizations {
			t.Fatalf("organization candidate heads = %d, want %d", candidateHeads, organizations)
		}
		if iteration > 0 {
			durations = append(durations, time.Since(started))
			statements = measuredStatements
		}
	}
	slices.Sort(durations)
	maximum := durations[len(durations)-1]
	return dispatchLaneMeasurement{
		Scenario: name, Rows: rows, Organizations: organizations, Scopes: scopes,
		CandidateHeads: candidateHeads, Statements: statements,
		MinimumMillis: milliseconds(durations[0]), MedianMillis: milliseconds(durations[len(durations)/2]),
		P95Millis: milliseconds(durations[percentileIndex(len(durations), 95)]), MaximumMillis: milliseconds(maximum),
		CandidateHeadsPerSecond: float64(candidateHeads) / maximum.Seconds(),
	}
}

func scanEveryDispatchLane(
	t *testing.T,
	ctx context.Context,
	store *RunPlacementStore,
) (int, int) {
	t.Helper()
	const organizationLimit = int32(32)
	const scopeLimit = int32(1)
	var cursors [runPlacementLaneCount]pgtype.UUID
	var complete [runPlacementLaneCount]bool
	remainingLanes := runPlacementLaneCount
	candidateHeads := 0
	statements := 0
	for remainingLanes > 0 {
		for lane := int16(0); lane < runPlacementLaneCount; lane++ {
			if complete[lane] {
				continue
			}
			organizations, err := store.ListOrganizations(ctx, lane, cursors[lane], organizationLimit+1)
			if err != nil {
				t.Fatal(err)
			}
			statements++
			selected := organizations[:min(len(organizations), int(organizationLimit))]
			if len(selected) == 0 {
				complete[lane] = true
				remainingLanes--
				continue
			}
			after := make([]runPlacementScopeCursor, len(selected))
			scopeRows, err := store.ListScopes(ctx, runPlacementScopeParams{
				organizations: selected,
				after:         after,
				limit:         scopeLimit,
			})
			if err != nil {
				t.Fatal(err)
			}
			statements++
			scopes := make([]runPlacementScope, 0, len(selected))
			for _, row := range scopeRows {
				scopes = append(scopes, row.scope)
			}
			if len(scopes) != len(selected) {
				t.Fatalf("lane %d scopes = %d, want %d", lane, len(scopes), len(selected))
			}
			limits := make([]int32, len(scopes))
			for i := range limits {
				limits[i] = 1
			}
			params := new(runPlacementCursor).candidateParams(scopes, limits)
			candidates, err := store.ListCandidates(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			statements++
			candidateHeads += len(candidates)
			cursors[lane] = selected[len(selected)-1]
			if len(organizations) <= int(organizationLimit) {
				complete[lane] = true
				remainingLanes--
			}
		}
	}
	return candidateHeads, statements
}
