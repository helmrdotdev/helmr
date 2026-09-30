package region

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
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
	blank := " "
	var input InputError
	if _, err := Update(t.Context(), q, "us-east", Patch{DisplayName: &blank}); !errors.As(err, &input) {
		t.Fatalf("blank display name error = %v, want InputError", err)
	}
	if _, err := Update(t.Context(), q, "missing", Patch{Location: &location}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update error = %v", err)
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
