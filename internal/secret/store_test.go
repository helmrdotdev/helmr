package secret

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestStoreEncryptsAndResolvesNames(t *testing.T) {
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
	if err := store.CheckNames(context.Background(), orgID, []string{"github-token"}); err != nil {
		t.Fatal(err)
	}
	if string(database.record.Ciphertext) == "secret-value" {
		t.Fatal("secret was stored in plaintext")
	}
	resolved, err := store.ResolveNames(context.Background(), orgID, []string{"github-token"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resolved["github-token"]) != "secret-value" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestValidateNameCorpus(t *testing.T) {
	valid := []string{"config-json", "0abc", "a.b", "A_B", "CON"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Fatalf("ValidateName(%q) = %v", name, err)
		}
	}
	invalid := []string{"", "-x", "_x", "bad/name", "bad name", strings.Repeat("a", 129)}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Fatalf("ValidateName(%q) succeeded", name)
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
