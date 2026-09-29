package controlplane

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

func TestGuestSecretCeilingRejectsRawAndWiderExistingTargets(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	source, target := pgvalue.UUID(fixture.computerIDs[0]), pgvalue.UUID(fixture.computerIDs[1])
	q := db.New(fixture.pool)
	dbtest.MustExec(t, t.Context(), fixture.pool, `UPDATE computer_secrets SET mode='protected',allowed_origins=ARRAY['https://api.example.com'],placeholder='hlmr_protected_'||repeat('a',64) WHERE computer_id=$1`, source)
	if err := authorizeComputerSecretTarget(t.Context(), q, source, target); !errors.Is(err, errComputerSecretUnavailable) {
		t.Fatalf("protected -> raw target=%v", err)
	}
	protected := secretbinding.Binding{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://api.example.com"}}}
	if err := authorizeComputerSecretCreate(t.Context(), q, source, pgvalue.UUID(fixture.environmentID), []secretbinding.Binding{protected}); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []secretbinding.Binding{
		{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "TOKEN", Mode: "raw"}},
		{Name: "API_TOKEN", File: &secretbinding.File{Path: "/run/secrets/key"}},
		{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://other.example.com"}}},
		{Name: "other-token", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://api.example.com"}}},
	} {
		if err := authorizeComputerSecretCreate(t.Context(), q, source, pgvalue.UUID(fixture.environmentID), []secretbinding.Binding{binding}); !errors.Is(err, errComputerSecretUnavailable) {
			t.Fatalf("create escalation=%v", err)
		}
	}
	dbtest.MustExec(t, t.Context(), fixture.pool, `UPDATE computer_secrets SET mode='protected',allowed_origins=ARRAY['https://api.example.com','https://other.example.com'],placeholder='hlmr_protected_'||repeat('b',64) WHERE computer_id=$1`, target)
	if err := authorizeComputerSecretTarget(t.Context(), q, source, target); !errors.Is(err, errComputerSecretUnavailable) {
		t.Fatalf("wider target=%v", err)
	}
	dbtest.MustExec(t, t.Context(), fixture.pool, `UPDATE computer_secrets SET allowed_origins=ARRAY['https://api.example.com'] WHERE computer_id=$1`, target)
	if err := authorizeComputerSecretTarget(t.Context(), q, source, target); err != nil {
		t.Fatal(err)
	}
	// Trusted authors may deliberately add raw authority at a distinct target.
	dbtest.MustExec(t, t.Context(), fixture.pool, `INSERT INTO computer_secrets(computer_id,environment_id,secret_id,placement_kind,placement_target,mode) SELECT computer_id,environment_id,secret_id,'env','RAW_TOKEN','raw' FROM computer_secrets WHERE computer_id=$1`, source)
	if err := authorizeComputerSecretCreate(t.Context(), q, source, pgvalue.UUID(fixture.environmentID), []secretbinding.Binding{{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "TOKEN", Mode: "raw"}}}); err != nil {
		t.Fatal(err)
	}
}
