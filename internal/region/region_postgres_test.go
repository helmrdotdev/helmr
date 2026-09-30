package region

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5/pgxpool"
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
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	q := db.New(database.Pool)
	if _, err := Create(t.Context(), q, Details{ID: "us-east", DisplayName: "US East", Location: "Virginia"}); err != nil {
		t.Fatal(err)
	}
	// The display name patch holds the row lock in an open transaction while
	// the location patch waits on it, so the location patch evaluates against
	// the committed display name rather than a value read before the wait.
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	displayName := "East"
	if _, err := Update(t.Context(), db.New(tx), "us-east", Patch{DisplayName: &displayName}); err != nil {
		t.Fatal(err)
	}
	location := "Ohio"
	done := make(chan error, 1)
	go func() {
		_, err := Update(t.Context(), q, "us-east", Patch{Location: &location})
		done <- err
	}()
	waitForRegionLockWaiter(t, database.Pool)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	found, err := Get(t.Context(), q, "us-east")
	if err != nil || found.DisplayName != "East" || found.Location != "Ohio" {
		t.Fatalf("region after concurrent patches = %+v, %v", found, err)
	}
}

func waitForRegionLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := pool.QueryRow(t.Context(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock'
			   AND query LIKE '%UPDATE regions%'
		)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("location patch never waited on the region row lock")
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

func TestEnsureConcurrentPostgres(t *testing.T) {
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
			errs <- Ensure(t.Context(), q, Details{ID: "shared", DisplayName: fmt.Sprintf("Caller %d", i)})
		}()
	}
	createErr := make(chan error, 1)
	go func() {
		<-start
		_, err := Create(t.Context(), q, Details{ID: "shared", DisplayName: "Created"})
		createErr <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := <-createErr; err != nil && !errors.Is(err, ErrExists) {
		t.Fatalf("concurrent Create error = %v", err)
	}
	regions, err := List(t.Context(), q)
	if err != nil || len(regions) != 1 || regions[0].ID != "shared" {
		t.Fatalf("regions after concurrent Ensure = %+v, %v", regions, err)
	}
}
