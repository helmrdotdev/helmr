package dispatch

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNewAuthorityRejectsNilPool(t *testing.T) {
	database, err := NewAuthority(nil)
	if database != nil {
		t.Fatalf("New(nil) database = %#v, want nil", database)
	}
	if !errors.Is(err, ErrNilPool) {
		t.Fatalf("New(nil) error = %v, want ErrNilPool", err)
	}
}

func TestNewAuthorityRetainsConcretePool(t *testing.T) {
	pool := &pgxpool.Pool{}
	database, err := NewAuthority(pool)
	if err != nil {
		t.Fatal(err)
	}
	if database.pool != pool {
		t.Fatalf("New(pool) retained %p, want %p", database.pool, pool)
	}
}
