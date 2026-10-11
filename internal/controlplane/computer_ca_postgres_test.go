package controlplane

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"

	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Direct instance fixtures deliberately attach protected bindings after their raw
// Computer insert. Production creation performs this write inside its insert tx.
func createTestComputerCA(t *testing.T, pool *pgxpool.Pool, store *secret.Store, environmentID, computerID uuid.UUID) secret.ProxyTrust {
	t.Helper()
	var created time.Time
	if err := pool.QueryRow(t.Context(), "SELECT created_at FROM computers WHERE id=$1", computerID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	trust, err := store.GenerateProxyTrust(environmentID, computerID, created)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pool.Exec(t.Context(), `UPDATE computers SET proxy_ca_certificate=$3,proxy_ca_private_key_nonce=$4,proxy_ca_private_key_ciphertext=$5,proxy_ca_not_after=$6 WHERE environment_id=$1 AND id=$2 AND proxy_ca_certificate IS NULL`, environmentID, computerID, trust.Certificate, trust.PrivateKeyNonce, trust.PrivateKeyCiphertext, trust.NotAfter)
	if err != nil || result.RowsAffected() != 1 {
		t.Fatalf("initialize fixture CA: %v", err)
	}
	return trust
}

func TestComputerCAMalformedPreparationFailsWithoutWrites(t *testing.T) {
	for _, mutation := range []string{"missing", "certificate", "nonce", "ciphertext"} {
		t.Run(mutation, func(t *testing.T) {
			f := newSnapshotFixture(t, 1, true)
			switch mutation {
			case "missing":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET proxy_ca_certificate=NULL,proxy_ca_not_after=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL WHERE id=$1", f.computer)
			case "certificate":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET proxy_ca_certificate=$2 WHERE id=$1", f.computer, []byte("invalid"))
			case "nonce":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET proxy_ca_private_key_nonce=$2 WHERE id=$1", f.computer, []byte("invalidnonce"))
			case "ciphertext":
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, "UPDATE computers SET proxy_ca_private_key_ciphertext=$2 WHERE id=$1", f.computer, []byte("invalid"))
			}
			var before, after string
			if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT xmin::text FROM computers WHERE id=$1", f.computer).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if r := f.invoke(t.Context(), false); r.Code == 200 {
					t.Fatal("malformed CA prepared")
				}
			}
			if err := f.fixture.Pool.QueryRow(t.Context(), "SELECT xmin::text FROM computers WHERE id=$1", f.computer).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("preparation repaired CA")
			}
		})
	}
	f := newSnapshotFixture(t, 1, false)
	for i, statement := range []string{
		"UPDATE computers SET proxy_ca_certificate=$2 WHERE id=$1",
		"UPDATE computers SET proxy_ca_certificate=$2,proxy_ca_private_key_nonce=$2,proxy_ca_private_key_ciphertext=$2,proxy_ca_not_after=now() WHERE id=$1",
	} {
		material := []byte("nonempty partial material")
		if i == 1 {
			material = []byte{}
		}
		if _, err := f.fixture.Pool.Exec(t.Context(), statement, f.computer, material); err == nil {
			t.Fatal("partial/empty material admitted")
		}
	}
}
