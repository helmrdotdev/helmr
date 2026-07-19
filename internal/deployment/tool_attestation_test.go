package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

func TestAuthenticateToolRegistry(t *testing.T) {
	registryBytes, registry := toolRegistryAttestationFixture(t)
	document := toolAttestationForTest(t, registry)
	virtual, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	entity, err := virtual.Attest(
		testRuntimeReleaseIdentity,
		releaseAttestationIssuer,
		canonicalToolAttestationForTest(t, document),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticateToolRegistry(
		registryBytes,
		registry,
		entity,
		virtual,
		releaseCertificateSANPattern,
	); err != nil {
		t.Fatal(err)
	}
}

func TestToolAttestationRejectsSubjectAndPredicateDrift(t *testing.T) {
	tests := map[string]func(*toolAttestationDocument){
		"missing subject": func(value *toolAttestationDocument) {
			value.Subject = value.Subject[:len(value.Subject)-1]
		},
		"subject digest": func(value *toolAttestationDocument) {
			value.Subject[0].Digest["sha256"] = strings.Repeat("0", 64)
		},
		"statement type": func(value *toolAttestationDocument) {
			value.Type += "/lookalike"
		},
		"predicate type": func(value *toolAttestationDocument) {
			value.PredicateType += "/lookalike"
		},
		"registry digest": func(value *toolAttestationDocument) {
			value.Predicate.Registry.Digest = "sha256:" + strings.Repeat("0", 64)
		},
		"corpus object": func(value *toolAttestationDocument) {
			value.Predicate.Corpora[0].Objects[0].SizeBytes++
		},
		"global object": func(value *toolAttestationDocument) {
			value.Predicate.Objects[0].SizeBytes++
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			registryBytes, registry := toolRegistryAttestationFixture(t)
			document := toolAttestationForTest(t, registry)
			mutate(&document)
			virtual, err := ca.NewVirtualSigstore()
			if err != nil {
				t.Fatal(err)
			}
			entity, err := virtual.Attest(
				testRuntimeReleaseIdentity,
				releaseAttestationIssuer,
				canonicalToolAttestationForTest(t, document),
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := authenticateToolRegistry(
				registryBytes,
				registry,
				entity,
				virtual,
				releaseCertificateSANPattern,
			); err == nil {
				t.Fatal("authenticateToolRegistry accepted attestation drift")
			}
		})
	}
}

func TestToolAttestationRejectsIdentityAndOpenPredicate(t *testing.T) {
	registryBytes, registry := toolRegistryAttestationFixture(t)
	document := toolAttestationForTest(t, registry)
	statement := canonicalToolAttestationForTest(t, document)

	virtual, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	entity, err := virtual.Attest(
		strings.Replace(testRuntimeReleaseIdentity, "release.yaml", "other.yaml", 1),
		releaseAttestationIssuer,
		statement,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticateToolRegistry(
		registryBytes,
		registry,
		entity,
		virtual,
		releaseCertificateSANPattern,
	); err == nil {
		t.Fatal("authenticateToolRegistry accepted another workflow")
	}

	statement = []byte(strings.Replace(
		string(statement),
		`"objects":`,
		`"extra":true,"objects":`,
		1,
	))
	entity, err = virtual.Attest(
		testRuntimeReleaseIdentity,
		releaseAttestationIssuer,
		statement,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticateToolRegistry(
		registryBytes,
		registry,
		entity,
		virtual,
		releaseCertificateSANPattern,
	); err == nil {
		t.Fatal("authenticateToolRegistry accepted an open predicate")
	}
}

func toolRegistryAttestationFixture(t *testing.T) ([]byte, *ToolRegistry) {
	t.Helper()
	manager, toolchain, _, toolset := testToolset(t)
	raw, err := CanonicalToolRegistry(
		[]ManagerRegistration{manager},
		[]Toolchain{toolchain},
		[]Toolset{toolset},
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := ParseToolRegistry(raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw, registry
}

func toolAttestationForTest(
	t *testing.T,
	registry *ToolRegistry,
) toolAttestationDocument {
	t.Helper()
	predicate, err := toolAttestationForRegistry(registry)
	if err != nil {
		t.Fatal(err)
	}
	subjects := []releaseAttestationSubject{{
		Name: "registry",
		Digest: map[string]string{
			"sha256": predicate.Registry.Digest[len("sha256:"):],
		},
	}}
	for _, corpus := range predicate.Corpora {
		raw, err := canonicalToolCorpusDocument(corpus)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(raw)
		subjects = append(subjects, releaseAttestationSubject{
			Name: "corpus/" + string(corpus.Architecture),
			Digest: map[string]string{
				"sha256": hex.EncodeToString(hash[:]),
			},
		})
	}
	for _, object := range predicate.Objects {
		subjects = append(subjects, releaseAttestationSubject{
			Name: "object/sha256/" + object.Digest[len("sha256:"):],
			Digest: map[string]string{
				"sha256": object.Digest[len("sha256:"):],
			},
		})
	}
	return toolAttestationDocument{
		Type:          RuntimeAttestationType,
		Subject:       subjects,
		PredicateType: ToolAttestationPredicateType,
		Predicate:     predicate,
	}
}

func canonicalToolAttestationForTest(
	t *testing.T,
	document toolAttestationDocument,
) []byte {
	t.Helper()
	raw, err := canonicalToolAttestationDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
