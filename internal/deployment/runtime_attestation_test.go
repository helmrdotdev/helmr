package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/testing/data"
	"google.golang.org/protobuf/encoding/protojson"
)

const testRuntimeReleaseIdentity = "https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/tags/v1.2.3-rc.1"

func TestAuthenticateRuntimeCatalog(t *testing.T) {
	catalogBytes, catalog := runtimeCatalogAttestationFixture(t)
	virtual, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	document := runtimeAttestationForTest(catalogBytes, catalog)
	entity, err := virtual.Attest(
		testRuntimeReleaseIdentity,
		releaseAttestationIssuer,
		canonicalRuntimeAttestationForTest(t, document),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := authenticateRuntimeCatalog(
		catalogBytes,
		catalog,
		entity,
		virtual,
	); err != nil {
		t.Fatalf("authenticate runtime catalog: %v", err)
	}
}

func TestRuntimeAttestationRejectsIdentityDrift(t *testing.T) {
	catalogBytes, catalog := runtimeCatalogAttestationFixture(t)
	document := runtimeAttestationForTest(catalogBytes, catalog)
	statement := canonicalRuntimeAttestationForTest(t, document)
	tests := map[string]struct {
		identity string
		issuer   string
	}{
		"issuer lookalike": {
			identity: testRuntimeReleaseIdentity,
			issuer:   releaseAttestationIssuer + ".example",
		},
		"fork": {
			identity: strings.Replace(testRuntimeReleaseIdentity, "helmrdotdev", "fork", 1),
			issuer:   releaseAttestationIssuer,
		},
		"other workflow": {
			identity: strings.Replace(testRuntimeReleaseIdentity, "release.yaml", "sdk-release.yaml", 1),
			issuer:   releaseAttestationIssuer,
		},
		"branch ref": {
			identity: strings.Replace(testRuntimeReleaseIdentity, "refs/tags/v1.2.3-rc.1", "refs/heads/main", 1),
			issuer:   releaseAttestationIssuer,
		},
		"SDK tag": {
			identity: strings.Replace(testRuntimeReleaseIdentity, "v1.2.3-rc.1", "sdk-v1.2.3", 1),
			issuer:   releaseAttestationIssuer,
		},
		"malformed tag": {
			identity: strings.Replace(testRuntimeReleaseIdentity, "v1.2.3-rc.1", "v01.2.3", 1),
			issuer:   releaseAttestationIssuer,
		},
		"trailing input": {
			identity: testRuntimeReleaseIdentity + "/extra",
			issuer:   releaseAttestationIssuer,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			virtual, err := ca.NewVirtualSigstore()
			if err != nil {
				t.Fatal(err)
			}
			entity, err := virtual.Attest(test.identity, test.issuer, statement)
			if err != nil {
				t.Fatal(err)
			}
			if err := authenticateRuntimeCatalog(
				catalogBytes,
				catalog,
				entity,
				virtual,
			); err == nil {
				t.Fatal("authenticateRuntimeCatalog accepted identity drift")
			}
		})
	}
}

func TestRuntimeAttestationExactBindsReleaseLineage(t *testing.T) {
	catalogBytes, catalog := runtimeCatalogAttestationFixture(t)
	predecessor := &RuntimeReleaseRef{
		Release:   "v1.2.2",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		SizeBytes: 123,
	}
	document := runtimeAttestationForTest(catalogBytes, catalog)
	document.Predicate.Predecessor = predecessor
	virtual, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	entity, err := virtual.Attest(
		testRuntimeReleaseIdentity,
		releaseAttestationIssuer,
		canonicalRuntimeAttestationForTest(t, document),
	)
	if err != nil {
		t.Fatal(err)
	}
	expectation := &runtimeAttestationExpectation{
		release:     "v1.2.3-rc.1",
		predecessor: predecessor,
	}
	if err := authenticateRuntimeCatalogWithExpectation(
		catalogBytes,
		catalog,
		entity,
		virtual,
		expectation,
	); err != nil {
		t.Fatal(err)
	}
	wrongRelease := *expectation
	wrongRelease.release = "v1.2.4"
	if err := authenticateRuntimeCatalogWithExpectation(
		catalogBytes,
		catalog,
		entity,
		virtual,
		&wrongRelease,
	); err == nil {
		t.Fatal("runtime attestation accepted the wrong exact release SAN")
	}
	wrongPredecessor := *predecessor
	wrongPredecessor.SizeBytes++
	wrongLineage := *expectation
	wrongLineage.predecessor = &wrongPredecessor
	if err := authenticateRuntimeCatalogWithExpectation(
		catalogBytes,
		catalog,
		entity,
		virtual,
		&wrongLineage,
	); err == nil {
		t.Fatal("runtime attestation accepted the wrong predecessor")
	}
}

func TestRuntimeAttestationRejectsSubjectAndPredicateDrift(t *testing.T) {
	catalogBytes, catalog := runtimeCatalogAttestationFixture(t)
	tests := map[string]func(*runtimeAttestationDocument){
		"missing subject": func(value *runtimeAttestationDocument) {
			value.Subject = value.Subject[:len(value.Subject)-1]
		},
		"duplicate subject": func(value *runtimeAttestationDocument) {
			value.Subject[1] = value.Subject[0]
		},
		"subject digest": func(value *runtimeAttestationDocument) {
			value.Subject[0].Digest["sha256"] = strings.Repeat("0", 64)
		},
		"statement type": func(value *runtimeAttestationDocument) {
			value.Type = "https://in-toto.io/Statement/v0.1"
		},
		"predicate type": func(value *runtimeAttestationDocument) {
			value.PredicateType += "/lookalike"
		},
		"predicate catalog digest": func(value *runtimeAttestationDocument) {
			value.Predicate.CatalogDigest = "sha256:" + strings.Repeat("0", 64)
		},
		"predicate catalog media type": func(value *runtimeAttestationDocument) {
			value.Predicate.CatalogMediaType += "; charset=json"
		},
		"predicate runtimes": func(value *runtimeAttestationDocument) {
			value.Predicate.Runtimes[0].SizeBytes++
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			virtual, err := ca.NewVirtualSigstore()
			if err != nil {
				t.Fatal(err)
			}
			document := runtimeAttestationForTest(catalogBytes, catalog)
			mutate(&document)
			entity, err := virtual.Attest(
				testRuntimeReleaseIdentity,
				releaseAttestationIssuer,
				canonicalRuntimeAttestationForTest(t, document),
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := authenticateRuntimeCatalog(
				catalogBytes,
				catalog,
				entity,
				virtual,
			); err == nil {
				t.Fatal("authenticateRuntimeCatalog accepted attestation drift")
			}
		})
	}
}

func TestRuntimeAttestationRejectsOpenPredicate(t *testing.T) {
	catalogBytes, catalog := runtimeCatalogAttestationFixture(t)
	document := runtimeAttestationForTest(catalogBytes, catalog)
	statement := canonicalRuntimeAttestationForTest(t, document)
	statement = []byte(strings.Replace(
		string(statement),
		`"runtimes":`,
		`"descriptors":[],"runtimes":`,
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
	if err := authenticateRuntimeCatalog(
		catalogBytes,
		catalog,
		entity,
		virtual,
	); err == nil {
		t.Fatal("authenticateRuntimeCatalog accepted predicate alias")
	}
}

func TestVerifyRuntimeCatalogRejectsMalformedTrustInputs(t *testing.T) {
	catalogBytes, _ := runtimeCatalogAttestationFixture(t)
	if _, err := VerifyRuntimeCatalog(catalogBytes, nil, nil); err == nil {
		t.Fatal("VerifyRuntimeCatalog accepted missing bundle and trust root")
	}
	if _, err := parseReleaseTrustedRoot([]byte(`{}`)); err == nil {
		t.Fatal("parseReleaseTrustedRoot accepted empty trusted root")
	}
}

func TestParseRuntimeBundleRequiresExactV03DSSE(t *testing.T) {
	protobufBundle := &protobundle.Bundle{
		MediaType: ReleaseBundleMediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_PublicKey{
				PublicKey: &protocommon.PublicKeyIdentifier{Hint: "test"},
			},
		},
		Content: &protobundle.Bundle_DsseEnvelope{
			DsseEnvelope: &protodsse.Envelope{
				Payload:     []byte(`{"_type":"test","predicate":{},"predicateType":"test","subject":[]}`),
				PayloadType: "application/vnd.in-toto+json",
				Signatures: []*protodsse.Signature{{
					Keyid: "test",
					Sig:   []byte("signature"),
				}},
			},
		},
	}
	raw, err := protojson.Marshal(protobufBundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseReleaseBundle(raw); err != nil {
		t.Fatalf("parseReleaseBundle rejected v0.3 DSSE: %v", err)
	}

	protobufBundle.MediaType = "application/vnd.dev.sigstore.bundle+json;version=0.3"
	raw, err = protojson.Marshal(protobufBundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseReleaseBundle(raw); err == nil {
		t.Fatal("parseReleaseBundle accepted a compatibility media type")
	}

	messageBundle := &protobundle.Bundle{
		MediaType:            ReleaseBundleMediaType,
		VerificationMaterial: protobufBundle.VerificationMaterial,
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{
					Algorithm: protocommon.HashAlgorithm_SHA2_256,
					Digest:    make([]byte, sha256.Size),
				},
				Signature: []byte("signature"),
			},
		},
	}
	raw, err = protojson.Marshal(messageBundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseReleaseBundle(raw); err == nil {
		t.Fatal("parseReleaseBundle accepted a message signature")
	}
}

func TestParseRuntimeTrustedRoot(t *testing.T) {
	raw, err := json.Marshal(data.TrustedRoot(t, "public-good.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseReleaseTrustedRoot(raw); err != nil {
		t.Fatalf("parseReleaseTrustedRoot rejected valid pinned roots: %v", err)
	}
}

func runtimeCatalogAttestationFixture(t *testing.T) ([]byte, *RuntimeCatalog) {
	t.Helper()
	first, second := testRuntimeCatalogDescriptors()
	raw, err := CanonicalRuntimeCatalog([]RuntimeDescriptor{first, second})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ParseRuntimeCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw, catalog
}

func runtimeAttestationForTest(
	catalogBytes []byte,
	catalog *RuntimeCatalog,
) runtimeAttestationDocument {
	catalogHash := sha256.Sum256(catalogBytes)
	subjects := []releaseAttestationSubject{{
		Name:   "catalog",
		Digest: map[string]string{"sha256": hex.EncodeToString(catalogHash[:])},
	}}
	for _, descriptor := range catalog.runtimes {
		hexDigest := descriptor.Digest[len("sha256:"):]
		subjects = append(subjects, releaseAttestationSubject{
			Name:   "runtime/sha256/" + hexDigest,
			Digest: map[string]string{"sha256": hexDigest},
		})
	}
	return runtimeAttestationDocument{
		Type:          RuntimeAttestationType,
		Subject:       subjects,
		PredicateType: RuntimeAttestationPredicateType,
		Predicate: runtimeAttestationPredicate{
			CatalogDigest:    "sha256:" + hex.EncodeToString(catalogHash[:]),
			CatalogMediaType: RuntimeCatalogMediaType,
			FormatVersion:    RuntimeAttestationFormatVersion,
			Runtimes:         append([]RuntimeDescriptor(nil), catalog.runtimes...),
		},
	}
}

func canonicalRuntimeAttestationForTest(
	t *testing.T,
	document runtimeAttestationDocument,
) []byte {
	t.Helper()
	raw, err := canonicalRuntimeAttestationDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
