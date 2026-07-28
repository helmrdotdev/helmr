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
	var fixedKey [workspace.FencingKeySize]byte
	copy(fixedKey[:], key)
	fingerprint := workspace.FencingKeyFingerprintForKey(fixedKey).String()
	keys, err := workspace.NewFencingKeys(
		fingerprint,
		map[string][]byte{fingerprint: key},
	)
	if err != nil {
		t.Fatal(err)
	}
	valid := RunPlacementPolicy{
		PreparationLimit: 1,
		ReservationTTL:   time.Minute,
		StartDeadline:    time.Minute,
		LeaseTTL:         2 * time.Minute,
	}
	if authority, err := NewRunAuthority(pool, workspace.FencingKeys{}, valid); authority != nil || err == nil {
		t.Fatalf("NewRunAuthority() without keys = (%#v, %v), want error", authority, err)
	}
	invalid := valid
	invalid.StartDeadline = 3 * time.Minute
	if authority, err := NewRunAuthority(pool, keys, invalid); authority != nil || err == nil {
		t.Fatalf("NewRunAuthority() with invalid policy = (%#v, %v), want error", authority, err)
	}
	authority, err := NewRunAuthority(pool, keys, valid)
	if err != nil {
		t.Fatal(err)
	}
	if authority.pool != pool ||
		authority.runPolicy != valid ||
		authority.fencingKeys.ActiveFingerprint() != keys.ActiveFingerprint() {
		t.Fatal("NewRunAuthority() did not retain exact Run grant authority")
	}
}
