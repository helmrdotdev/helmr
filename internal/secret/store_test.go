package secret

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestStoreEncryptsAndResolvesBindings(t *testing.T) {
	key, err := KeyFromBase64(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeSecretDB{}
	store, err := New(database, DefaultKeyID, key)
	if err != nil {
		t.Fatal(err)
	}
	orgID := ids.New()
	if _, err := store.Put(context.Background(), orgID, "github-token", []byte("secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background(), orgID, api.SecretBindings{"TOKEN": "vault:github-token"}); err != nil {
		t.Fatal(err)
	}
	if string(database.record.Ciphertext) == "secret-value" {
		t.Fatal("secret was stored in plaintext")
	}
	resolved, err := store.Resolve(context.Background(), orgID, api.SecretBindings{"TOKEN": "vault:github-token"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resolved["TOKEN"]) != "secret-value" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestValidateBindingsRequiresVaultURI(t *testing.T) {
	for _, binding := range []string{"github-token", "env:TOKEN", "file:/tmp/token", "vault:/token", "vault:token?version=1"} {
		err := ValidateBindings(api.SecretBindings{"TOKEN": binding})
		if err == nil {
			t.Fatalf("expected invalid binding for %q", binding)
		}
	}
}

func TestKeyFromBase64RequiresAES256Key(t *testing.T) {
	_, err := KeyFromBase64(base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if err == nil {
		t.Fatal("expected short key error")
	}
}

type fakeSecretDB struct {
	db.Querier
	record db.Secret
}

func (f *fakeSecretDB) GetDefaultProjectEnvironment(context.Context, pgtype.UUID) (db.GetDefaultProjectEnvironmentRow, error) {
	return db.GetDefaultProjectEnvironmentRow{
		ProjectID:     ids.ToPG(ids.DefaultOrgID),
		EnvironmentID: ids.ToPG(ids.DefaultOrgID),
	}, nil
}

func (f *fakeSecretDB) UpsertScopedSecret(_ context.Context, arg db.UpsertScopedSecretParams) (db.Secret, error) {
	f.record = db.Secret{
		ID:            arg.ID,
		OrgID:         arg.OrgID,
		ProjectID:     arg.ProjectID,
		EnvironmentID: arg.EnvironmentID,
		Name:          arg.Name,
		KeyID:         arg.KeyID,
		Nonce:         arg.Nonce,
		Ciphertext:    arg.Ciphertext,
	}
	return f.record, nil
}

func (f *fakeSecretDB) GetScopedSecretByName(_ context.Context, arg db.GetScopedSecretByNameParams) (db.Secret, error) {
	if f.record.OrgID != arg.OrgID || f.record.ProjectID != arg.ProjectID || f.record.EnvironmentID != arg.EnvironmentID || f.record.Name != arg.Name {
		return db.Secret{}, pgx.ErrNoRows
	}
	return f.record, nil
}
