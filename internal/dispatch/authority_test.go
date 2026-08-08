package dispatch

import (
	"bytes"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNewAuthorityRejectsNilPool(t *testing.T) {
	database, err := newAuthority(nil)
	if database != nil {
		t.Fatalf("New(nil) database = %#v, want nil", database)
	}
	if !errors.Is(err, ErrNilPool) {
		t.Fatalf("New(nil) error = %v, want ErrNilPool", err)
	}
}

func TestNewAuthorityRetainsConcretePool(t *testing.T) {
	pool := &pgxpool.Pool{}
	database, err := newAuthority(pool)
	if err != nil {
		t.Fatal(err)
	}
	if database.pool != pool {
		t.Fatalf("New(pool) retained %p, want %p", database.pool, pool)
	}
}

func TestNewRunAuthorityRequiresFencingAuthority(t *testing.T) {
	pool := &pgxpool.Pool{}
	key := bytes.Repeat([]byte{1}, workspace.FencingKeySize)
	fencingKey, err := workspace.NewFencingKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if authority, err := NewRunAuthority(pool, workspace.FencingKey{}); authority != nil || err == nil {
		t.Fatalf("NewRunAuthority() without keys = (%#v, %v), want error", authority, err)
	}
	authority, err := NewRunAuthority(pool, fencingKey)
	if err != nil {
		t.Fatal(err)
	}
	if authority.pool != pool || !authority.fencingKey.Valid() {
		t.Fatal("NewRunAuthority() did not retain exact Run grant authority")
	}
}
