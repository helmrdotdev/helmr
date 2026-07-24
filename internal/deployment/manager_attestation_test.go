package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

func TestAuthenticateManagerCatalog(t *testing.T) {
	bun := testManager(PackageManagerBun, ArchitectureX8664)
	npm := testManager(PackageManagerNPM, ArchitectureX8664)
	catalogBytes, err := CanonicalManagerCatalog([]Manager{bun, npm})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ParseManagerCatalog(catalogBytes)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(catalogBytes)
	raw, err := json.Marshal(managerAttestationDocument{
		Type: RuntimeAttestationType,
		Subject: []releaseAttestationSubject{{
			Name: "manager-release/catalog.json",
			Digest: map[string]string{
				"sha256": hex.EncodeToString(hash[:]),
			},
		}},
		PredicateType: ManagerAttestationPredicateType,
		Predicate: managerAttestationPredicate{
			CatalogDigest:    "sha256:" + hex.EncodeToString(hash[:]),
			CatalogMediaType: ManagerCatalogMediaType,
			FormatVersion:    ManagerCatalogFormatVersion,
			Managers:         []Manager{bun, npm},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	statement, err := jsoncanon.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	virtual, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	entity, err := virtual.Attest(
		testRuntimeReleaseIdentity,
		releaseAttestationIssuer,
		statement,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticateManagerCatalog(
		catalogBytes,
		catalog,
		entity,
		virtual,
	); err != nil {
		t.Fatal(err)
	}
}
