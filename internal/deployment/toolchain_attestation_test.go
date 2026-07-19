package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

func TestAuthenticateToolchainCatalog(t *testing.T) {
	catalogBytes, catalog := toolchainCatalogAttestationFixture(t)
	virtual, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	entity, err := virtual.Attest(
		testRuntimeReleaseIdentity,
		releaseAttestationIssuer,
		canonicalToolchainAttestationForTest(
			t,
			toolchainAttestationForTest(catalogBytes, catalog),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticateToolchainCatalog(
		catalogBytes,
		catalog,
		entity,
		virtual,
		nil,
	); err != nil {
		t.Fatal(err)
	}
}

func TestToolchainAttestationExactBindsReleaseLineage(t *testing.T) {
	catalogBytes, catalog := toolchainCatalogAttestationFixture(t)
	predecessor := &RuntimeReleaseRef{
		Release:   "v1.2.2",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		SizeBytes: 123,
	}
	document := toolchainAttestationForTest(catalogBytes, catalog)
	document.Predicate.Predecessor = predecessor
	virtual, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	entity, err := virtual.Attest(
		testRuntimeReleaseIdentity,
		releaseAttestationIssuer,
		canonicalToolchainAttestationForTest(t, document),
	)
	if err != nil {
		t.Fatal(err)
	}
	expectation := &toolchainAttestationExpectation{
		release:     "v1.2.3-rc.1",
		predecessor: predecessor,
	}
	if err := authenticateToolchainCatalog(
		catalogBytes,
		catalog,
		entity,
		virtual,
		expectation,
	); err != nil {
		t.Fatal(err)
	}
	wrong := *expectation
	wrong.release = "v1.2.4"
	if err := authenticateToolchainCatalog(
		catalogBytes,
		catalog,
		entity,
		virtual,
		&wrong,
	); err == nil {
		t.Fatal("standard-toolchain attestation accepted another release")
	}
}

func TestToolchainAttestationRejectsSubjectAndPredicateDrift(t *testing.T) {
	catalogBytes, catalog := toolchainCatalogAttestationFixture(t)
	tests := map[string]func(*toolchainAttestationDocument){
		"missing subject": func(value *toolchainAttestationDocument) {
			value.Subject = value.Subject[:len(value.Subject)-1]
		},
		"duplicate subject": func(value *toolchainAttestationDocument) {
			value.Subject[1] = value.Subject[0]
		},
		"predicate type": func(value *toolchainAttestationDocument) {
			value.PredicateType += "/lookalike"
		},
		"catalog digest": func(value *toolchainAttestationDocument) {
			value.Predicate.CatalogDigest = "sha256:" + strings.Repeat("0", 64)
		},
		"catalog media type": func(value *toolchainAttestationDocument) {
			value.Predicate.CatalogMediaType += "; charset=json"
		},
		"toolchains": func(value *toolchainAttestationDocument) {
			value.Predicate.Toolchains[0].ToolchainClosure.SizeBytes++
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			document := toolchainAttestationForTest(catalogBytes, catalog)
			mutate(&document)
			virtual, err := ca.NewVirtualSigstore()
			if err != nil {
				t.Fatal(err)
			}
			entity, err := virtual.Attest(
				testRuntimeReleaseIdentity,
				releaseAttestationIssuer,
				canonicalToolchainAttestationForTest(t, document),
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := authenticateToolchainCatalog(
				catalogBytes,
				catalog,
				entity,
				virtual,
				nil,
			); err == nil {
				t.Fatal("standard-toolchain attestation accepted drift")
			}
		})
	}
}

func TestToolchainAttestationRejectsOpenPredicate(t *testing.T) {
	catalogBytes, catalog := toolchainCatalogAttestationFixture(t)
	statement := canonicalToolchainAttestationForTest(
		t,
		toolchainAttestationForTest(catalogBytes, catalog),
	)
	statement = []byte(strings.Replace(
		string(statement),
		`"toolchains":`,
		`"descriptors":[],"toolchains":`,
		1,
	))
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
	if err := authenticateToolchainCatalog(
		catalogBytes,
		catalog,
		entity,
		virtual,
		nil,
	); err == nil {
		t.Fatal("standard-toolchain attestation accepted a predicate alias")
	}
}

func toolchainCatalogAttestationFixture(t *testing.T) ([]byte, *ToolchainCatalog) {
	t.Helper()
	runtime := testRuntimeDescriptor()
	toolchain, _ := testToolchainForRuntime(t, runtime)
	raw, err := CanonicalToolchainCatalog([]Toolchain{toolchain})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ParseToolchainCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw, catalog
}

func toolchainAttestationForTest(
	catalogBytes []byte,
	catalog *ToolchainCatalog,
) toolchainAttestationDocument {
	catalogHash := sha256.Sum256(catalogBytes)
	subjects := []releaseAttestationSubject{{
		Name:   "catalog",
		Digest: map[string]string{"sha256": hex.EncodeToString(catalogHash[:])},
	}}
	seen := make(map[string]struct{}, len(catalog.toolchains))
	for _, toolchain := range catalog.toolchains {
		hexDigest := toolchain.ToolchainClosure.Digest[len("sha256:"):]
		if _, ok := seen[hexDigest]; ok {
			continue
		}
		seen[hexDigest] = struct{}{}
		subjects = append(subjects, releaseAttestationSubject{
			Name:   "toolchain/sha256/" + hexDigest,
			Digest: map[string]string{"sha256": hexDigest},
		})
	}
	return toolchainAttestationDocument{
		Type:          RuntimeAttestationType,
		Subject:       subjects,
		PredicateType: ToolchainAttestationPredicateType,
		Predicate: toolchainAttestationPredicate{
			CatalogDigest:    "sha256:" + hex.EncodeToString(catalogHash[:]),
			CatalogMediaType: ToolchainCatalogMediaType,
			FormatVersion:    ToolchainCatalogFormatVersion,
			Toolchains:       append([]Toolchain(nil), catalog.toolchains...),
		},
	}
}

func canonicalToolchainAttestationForTest(
	t *testing.T,
	document toolchainAttestationDocument,
) []byte {
	t.Helper()
	raw, err := canonicalToolchainAttestationDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
