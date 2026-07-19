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

func TestNewBuildAuthorityRequiresRegistryDigest(t *testing.T) {
	pool := &pgxpool.Pool{}
	authority, err := NewBuildAuthority(pool, make([]byte, 31))
	if authority != nil {
		t.Fatalf("NewBuildAuthority() authority = %#v, want nil", authority)
	}
	if err == nil {
		t.Fatal("NewBuildAuthority() error = nil, want digest error")
	}
}
