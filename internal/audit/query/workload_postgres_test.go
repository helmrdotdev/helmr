package auditquery

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
)

// This is a reproducible local capacity sample, not a deployment sizing promise.
func TestAuditRepresentativeConcurrentWorkload(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	org, user := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO organizations(id,slug,name) VALUES($1,'workload','Workload')`, org)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name,admin) VALUES($1,'Workload auditor',true)`, user)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	const writers, perWriter = 4, 2000
	var wg sync.WaitGroup
	errors := make(chan error, writers)
	samples := make(chan []time.Duration, writers)
	start := time.Now()
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			latencies := make([]time.Duration, 0, perWriter)
			for range perWriter {
				before := time.Now()
				err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error {
					return audit.Append(t.Context(), tx, policy, audit.Event{Evidence: audit.Internal(audit.ControlPlane), OrgID: org, Action: "command.completed", TargetKind: "command", TargetID: uuid.NewV7(), Details: audit.LifecycleDetails{}})
				})
				if err != nil {
					errors <- err
					return
				}
				latencies = append(latencies, time.Since(before))
			}
			samples <- latencies
		}()
	}
	wg.Wait()
	close(errors)
	close(samples)
	for err := range errors {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	var times []time.Duration
	for sample := range samples {
		times = append(times, sample...)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	var rows, tableBytes, indexBytes int64
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*),pg_table_size('audit_events'),pg_indexes_size('audit_events') FROM audit_events`).Scan(&rows, &tableBytes, &indexBytes); err != nil {
		t.Fatal(err)
	}
	if rows != writers*perWriter {
		t.Fatal("admitted facts missing", rows)
	}
	t.Logf("sample writers=%d rows=%d elapsed_ms=%.3f append_p50_ms=%.3f append_p95_ms=%.3f append_p99_ms=%.3f table_bytes=%d index_bytes=%d", writers, rows, float64(elapsed.Microseconds())/1000, float64(times[len(times)/2].Microseconds())/1000, float64(times[len(times)*95/100].Microseconds())/1000, float64(times[len(times)*99/100].Microseconds())/1000, tableBytes, indexBytes)
	dbtest.MustExec(t, t.Context(), d.Pool, `ANALYZE audit_events`)
	filter := SearchFilter{OrgID: org, From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Minute), Limit: 100}
	principal := auth.Principal{Kind: auth.PrincipalKindSession, UserID: user, Admin: true, SessionID: uuid.NewV7()}
	for _, action := range []string{"command.completed", "command.failed"} {
		filter.Actions = []string{action}
		started := time.Now()
		result, err := SearchAdmin(t.Context(), d.Pool, policy, principal, filter)
		if err != nil {
			t.Fatal(err)
		}
		if action == "command.completed" && (len(result.Events) != 100 || result.NextBefore == nil) {
			t.Fatal("page bound", len(result.Events))
		}
		if action == "command.failed" && len(result.Events) != 0 {
			t.Fatal("sparse filter returned content")
		}
		t.Logf("search action=%s rows=%d elapsed_ms=%.3f", action, len(result.Events), float64(time.Since(started).Microseconds())/1000)
		var plan json.RawMessage
		if err := d.Pool.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+searchSQL, org, true, []uuid.UUID{}, nil, filter.From, filter.To, nil, nil, "", nil, []string{action}, "", nil, "", nil, "", "", 101, nil, nil, true, audit.PlatformActions()).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		t.Logf("query_plan action=%s %s", action, plan)
	}
	filter.Actions, filter.ActorKind, filter.ActorID = nil, "user", user
	actorRows, err := SearchAdmin(t.Context(), d.Pool, policy, principal, filter)
	if err != nil || len(actorRows.Events) != 2 {
		t.Fatal("actor search", len(actorRows.Events), err)
	}
	var actorPlan json.RawMessage
	if err := d.Pool.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+searchSQL, org, true, []uuid.UUID{}, nil, filter.From, filter.To, nil, nil, "user", user, []string{}, "", nil, "", nil, "", "", 101, nil, nil, true, audit.PlatformActions()).Scan(&actorPlan); err != nil {
		t.Fatal(err)
	}
	t.Logf("actor_query_plan %s", actorPlan)
	if !strings.Contains(string(actorPlan), "audit_events_actor") {
		t.Fatal("actor lookup did not use its index", string(actorPlan))
	}
	filter.ActorKind, filter.ActorID = "", uuid.Nil()
	// Purge and searches overlap: neither can disclose expired rows, and the
	// summary counts must equal the rows actually removed across competing jobs.
	dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE audit_events SET recorded_at=statement_timestamp()-interval '2 hours',expires_at=statement_timestamp()-interval '1 hour' WHERE action='command.completed'`)
	filter.From = time.Now().Add(-3 * time.Hour)
	filter.Actions = []string{"command.completed"}
	counts := make(chan int64, 2)
	purgeErrors := make(chan error, 2)
	start = time.Now()
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := audit.Purge(t.Context(), d.Pool, policy, 8)
			if err != nil {
				purgeErrors <- err
				return
			}
			counts <- r.Deleted
		}()
	}
	for range 4 {
		r, err := SearchAdmin(t.Context(), d.Pool, policy, principal, filter)
		if err != nil || len(r.Events) != 0 {
			t.Fatal("expired concurrent disclosure", len(r.Events), err)
		}
	}
	wg.Wait()
	close(counts)
	close(purgeErrors)
	for err := range purgeErrors {
		t.Fatal(err)
	}
	var removed int64
	for count := range counts {
		removed += count
	}
	if removed != writers*perWriter {
		t.Fatal("concurrent purge accounting", removed)
	}
	var accounted int64
	if err := d.Pool.QueryRow(context.Background(), `SELECT sum((details->>'count')::bigint) FROM audit_events WHERE action='audit.purge_completed'`).Scan(&accounted); err != nil || accounted != removed {
		t.Fatal("receipt accounting", accounted, err)
	}
	t.Logf("purge workers=2 rows=%d elapsed_ms=%.3f", removed, float64(time.Since(start).Microseconds())/1000)
}
