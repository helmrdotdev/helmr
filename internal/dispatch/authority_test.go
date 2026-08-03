package dispatch

import (
	"bytes"
	"errors"
	"testing"
	"time"

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

func TestNewRunAuthorityRequiresFencingAuthorityAndValidPolicy(t *testing.T) {
	pool := &pgxpool.Pool{}
	key := bytes.Repeat([]byte{1}, workspace.FencingKeySize)
	fencingKey, err := workspace.NewFencingKey(key)
	if err != nil {
		t.Fatal(err)
	}
	valid := RunPlacementPolicy{
		PreparationLimit: 1,
		ReservationTTL:   time.Minute,
		StartDeadline:    time.Minute,
		LeaseTTL:         2 * time.Minute,
	}
	if authority, err := NewRunAuthority(pool, workspace.FencingKey{}, valid); authority != nil || err == nil {
		t.Fatalf("NewRunAuthority() without keys = (%#v, %v), want error", authority, err)
	}
	invalid := valid
	invalid.StartDeadline = 3 * time.Minute
	if authority, err := NewRunAuthority(pool, fencingKey, invalid); authority != nil || err == nil {
		t.Fatalf("NewRunAuthority() with invalid policy = (%#v, %v), want error", authority, err)
	}
	authority, err := NewRunAuthority(pool, fencingKey, valid)
	if err != nil {
		t.Fatal(err)
	}
	if authority.pool != pool || authority.runPolicy != valid || !authority.fencingKey.Valid() {
		t.Fatal("NewRunAuthority() did not retain exact Run grant authority")
	}
}
