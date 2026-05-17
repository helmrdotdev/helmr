package control

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestBootstrapSkipsSetupWhenDisabled(t *testing.T) {
	store := &bootstrapStore{}

	result, err := Bootstrap(context.Background(), store, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.SetupRequired {
		t.Fatal("setup should not be required")
	}
	if !store.organizationEnsured {
		t.Fatal("default organization was not ensured")
	}
	if store.ownerChecked {
		t.Fatal("owner should not be checked when setup is disabled")
	}
}

func TestBootstrapRequiresSetupWhenOwnerMissing(t *testing.T) {
	store := &bootstrapStore{}

	result, err := Bootstrap(context.Background(), store, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SetupRequired {
		t.Fatal("setup should be required")
	}
	if !store.organizationEnsured {
		t.Fatal("default organization was not ensured")
	}
	if !store.ownerChecked {
		t.Fatal("owner was not checked")
	}
}

func TestBootstrapSkipsSetupWhenOwnerExists(t *testing.T) {
	store := &bootstrapStore{ownerExists: true}

	result, err := Bootstrap(context.Background(), store, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.SetupRequired {
		t.Fatal("setup should not be required")
	}
}

type bootstrapStore struct {
	organizationEnsured bool
	ownerChecked        bool
	ownerExists         bool
}

func (s *bootstrapStore) EnsureDefaultOrganization(context.Context, pgtype.UUID) error {
	s.organizationEnsured = true
	return nil
}

func (s *bootstrapStore) OwnerExists(context.Context, pgtype.UUID) (bool, error) {
	s.ownerChecked = true
	return s.ownerExists, nil
}
