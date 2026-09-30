package computer

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

func TestSecretCeilingRejectsRawAndWiderTargets(t *testing.T) {
	f := newFixture(t)
	source, target := pgvalue.UUID(f.insertComputer(t, "source")), pgvalue.UUID(f.insertComputer(t, "target"))
	environment := pgvalue.UUID(f.EnvironmentID)
	q := db.New(f.Pool)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_secrets SET mode='protected',allowed_origins=ARRAY['https://api.example.com'],placeholder='hlmr_protected_'||repeat('a',64) WHERE computer_id=$1`, source)
	if err := CheckSecretTarget(t.Context(), q, source, target); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("protected -> raw target=%v", err)
	}
	if err := LockSecretsWithinCeiling(t.Context(), q, source, environment, []secretbinding.Binding{protectedBinding("TOKEN", "https://api.example.com")}); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []secretbinding.Binding{
		rawBinding("TOKEN"),
		{Name: "API_TOKEN", File: &secretbinding.File{Path: "/run/secrets/key"}},
		protectedBinding("TOKEN", "https://other.example.com"),
		{Name: "other-token", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://api.example.com"}}},
	} {
		if err := LockSecretsWithinCeiling(t.Context(), q, source, environment, []secretbinding.Binding{binding}); !errors.Is(err, ErrSecretUnavailable) {
			t.Fatalf("create escalation=%v", err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_secrets SET mode='protected',allowed_origins=ARRAY['https://api.example.com','https://other.example.com'],placeholder='hlmr_protected_'||repeat('b',64) WHERE computer_id=$1`, target)
	if err := CheckSecretTarget(t.Context(), q, source, target); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("wider target=%v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_secrets SET allowed_origins=ARRAY['https://api.example.com'] WHERE computer_id=$1`, target)
	if err := CheckSecretTarget(t.Context(), q, source, target); err != nil {
		t.Fatal(err)
	}
	// Trusted authors may deliberately add raw authority at a distinct target.
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secrets(computer_id,environment_id,secret_id,placement_kind,placement_target,mode) SELECT computer_id,environment_id,secret_id,'env','RAW_TOKEN','raw' FROM computer_secrets WHERE computer_id=$1`, source)
	if err := LockSecretsWithinCeiling(t.Context(), q, source, environment, []secretbinding.Binding{rawBinding("TOKEN")}); err != nil {
		t.Fatal(err)
	}
}
