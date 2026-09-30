package region

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
)

func newRegionQueries(t *testing.T) *db.Queries {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	return db.New(database.Pool)
}

func TestCreateGetListPostgres(t *testing.T) {
	q := newRegionQueries(t)
	created, err := Create(t.Context(), q, Details{ID: "us-east", DisplayName: " US East ", Location: " Virginia "})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "us-east" || created.DisplayName != "US East" || created.Location != "Virginia" {
		t.Fatalf("created = %+v", created)
	}
	if _, err := Create(t.Context(), q, Details{ID: "us-east", DisplayName: "Again"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create error = %v", err)
	}
	var input InputError
	for _, bad := range []Details{{ID: " us-west", DisplayName: "West"}, {ID: "us-west", DisplayName: " "}} {
		if _, err := Create(t.Context(), q, bad); !errors.As(err, &input) {
			t.Fatalf("Create(%+v) error = %v, want InputError", bad, err)
		}
	}
	found, err := Get(t.Context(), q, "us-east")
	if err != nil || found != created {
		t.Fatalf("Get = %+v, %v", found, err)
	}
	if _, err := Get(t.Context(), q, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get error = %v", err)
	}
	regions, err := List(t.Context(), q)
	if err != nil || len(regions) != 1 || regions[0] != created {
		t.Fatalf("List = %+v, %v", regions, err)
	}
}

func TestUpdatePostgres(t *testing.T) {
	q := newRegionQueries(t)
	if _, err := Create(t.Context(), q, Details{ID: "us-east", DisplayName: "US East", Location: "Virginia"}); err != nil {
		t.Fatal(err)
	}
	location := " Ohio "
	updated, err := Update(t.Context(), q, "us-east", Patch{Location: &location})
	if err != nil || updated.DisplayName != "US East" || updated.Location != "Ohio" {
		t.Fatalf("location update = %+v, %v", updated, err)
	}
	displayName := " East "
	updated, err = Update(t.Context(), q, "us-east", Patch{DisplayName: &displayName})
	if err != nil || updated.DisplayName != "East" || updated.Location != "Ohio" {
		t.Fatalf("display name update = %+v, %v", updated, err)
	}
	empty := ""
	updated, err = Update(t.Context(), q, "us-east", Patch{Location: &empty})
	if err != nil || updated.DisplayName != "East" || updated.Location != "" {
		t.Fatalf("empty location update = %+v, %v", updated, err)
	}
	blank := " "
	var input InputError
	if _, err := Update(t.Context(), q, "us-east", Patch{DisplayName: &blank, Location: &location}); !errors.As(err, &input) {
		t.Fatalf("blank display name error = %v, want InputError", err)
	}
	found, err := Get(t.Context(), q, "us-east")
	if err != nil || found.DisplayName != "East" || found.Location != "" {
		t.Fatalf("region after rejected update = %+v, %v", found, err)
	}
	if _, err := Update(t.Context(), q, "missing", Patch{Location: &location}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update error = %v", err)
	}
	if _, err := Update(t.Context(), q, "missing", Patch{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing empty update error = %v", err)
	}
}

func TestUpdateConcurrentDisjointFieldsPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	holder, waiter, observer := connect(ctx, t, database.DSN), connect(ctx, t, database.DSN), connect(ctx, t, database.DSN)
	holderPID, waiterPID := backendPID(ctx, t, holder), backendPID(ctx, t, waiter)
	if _, err := Create(ctx, db.New(holder), Details{ID: "us-east", DisplayName: "US East", Location: "Virginia"}); err != nil {
		t.Fatal(err)
	}
	// The display name patch holds the row lock in an open transaction while
	// the location patch waits on it, so the location patch evaluates against
	// the committed display name rather than a value read before the wait.
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	displayName := "East"
	if _, err := Update(ctx, db.New(tx), "us-east", Patch{DisplayName: &displayName}); err != nil {
		t.Fatal(err)
	}
	location := "Ohio"
	done := make(chan error, 1)
	go func() {
		_, err := Update(ctx, db.New(waiter), "us-east", Patch{Location: &location})
		done <- err
	}()
	waitUntilBlocked(ctx, t, observer, waiterPID, holderPID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := receive(ctx, t, done); err != nil {
		t.Fatal(err)
	}
	found, err := Get(ctx, db.New(observer), "us-east")
	if err != nil || found.DisplayName != "East" || found.Location != "Ohio" {
		t.Fatalf("region after concurrent patches = %+v, %v", found, err)
	}
}

// connect opens a dedicated connection so a blocked statement and the
// observer that watches it never compete for pool capacity.
func connect(ctx context.Context, t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func backendPID(ctx context.Context, t *testing.T, conn *pgx.Conn) int32 {
	t.Helper()
	var pid int32
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitUntilBlocked returns once the waiter backend is blocked by the holder
// backend.
func waitUntilBlocked(ctx context.Context, t *testing.T, observer *pgx.Conn, waiter, holder int32) {
	t.Helper()
	for {
		var blocked bool
		if err := observer.QueryRow(ctx, `SELECT $2::integer = ANY(pg_blocking_pids($1::integer))`, waiter, holder).Scan(&blocked); err != nil {
			t.Fatalf("observe backend %d blocked by %d: %v", waiter, holder, err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("backend %d was never blocked by %d: %v", waiter, holder, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func receive[T any](ctx context.Context, t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatalf("result did not arrive: %v", ctx.Err())
		panic("unreachable")
	}
}

func TestEnsurePostgres(t *testing.T) {
	q := newRegionQueries(t)
	if err := Ensure(t.Context(), q, Details{ID: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(t.Context(), q, Details{ID: "local", DisplayName: "Changed"}); err != nil {
		t.Fatal(err)
	}
	found, err := Get(t.Context(), q, "local")
	if err != nil || found.DisplayName != "local" {
		t.Fatalf("ensured region = %+v, %v", found, err)
	}
	var input InputError
	if err := Ensure(t.Context(), q, Details{ID: ""}); !errors.As(err, &input) {
		t.Fatalf("empty ID error = %v, want InputError", err)
	}
}

func TestEnsureWaitsForConcurrentCreatePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	database := dbtest.Open(t)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	holder, waiter, observer := connect(ctx, t, database.DSN), connect(ctx, t, database.DSN), connect(ctx, t, database.DSN)
	holderPID, waiterPID := backendPID(ctx, t, holder), backendPID(ctx, t, waiter)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	winner, err := db.New(tx).CreateRegion(ctx, db.CreateRegionParams{ID: "shared", DisplayName: "Winner", Location: "Virginia"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- Ensure(ctx, db.New(waiter), Details{ID: "shared", DisplayName: "Loser", Location: "Ohio"})
	}()
	waitUntilBlocked(ctx, t, observer, waiterPID, holderPID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := receive(ctx, t, done); err != nil {
		t.Fatalf("Ensure after concurrent create = %v", err)
	}
	found, err := Get(ctx, db.New(observer), "shared")
	if err != nil || found != winner {
		t.Fatalf("region = %+v, %v; want %+v", found, err, winner)
	}
}

func TestEnsureConcurrentPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	q := newRegionQueries(t)
	const callers = 16
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- Ensure(ctx, q, Details{ID: "shared", DisplayName: fmt.Sprintf("Caller %d", i)})
		}()
	}
	createErr := make(chan error, 1)
	go func() {
		<-start
		_, err := Create(ctx, q, Details{ID: "shared", DisplayName: "Created"})
		createErr <- err
	}()
	close(start)
	finished := make(chan struct{}, 1)
	go func() {
		wg.Wait()
		finished <- struct{}{}
	}()
	receive(ctx, t, finished)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := receive(ctx, t, createErr); err != nil && !errors.Is(err, ErrExists) {
		t.Fatalf("concurrent Create error = %v", err)
	}
	regions, err := List(ctx, q)
	if err != nil || len(regions) != 1 || regions[0].ID != "shared" {
		t.Fatalf("regions after concurrent Ensure = %+v, %v", regions, err)
	}
}
