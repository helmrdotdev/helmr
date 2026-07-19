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

func TestNewBuildAuthorityRequiresAuthenticatedCatalogIdentity(t *testing.T) {
	pool := &pgxpool.Pool{}
	authority, err := NewBuildAuthority(pool, nil, func(string) error { return nil })
	if authority != nil || err == nil {
		t.Fatalf("NewBuildAuthority() = (%#v, %v), want error", authority, err)
	}
}

func TestNewBuildAuthorityRequiresResolver(t *testing.T) {
	pool := &pgxpool.Pool{}
	authority, err := NewBuildAuthority(pool, make([]byte, 32), nil)
	if authority != nil || err == nil {
		t.Fatalf("NewBuildAuthority() = (%#v, %v), want error", authority, err)
	}
}
