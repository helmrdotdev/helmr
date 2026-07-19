package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const ToolchainAttestationPredicateType = "https://helmr.dev/attestations/standard-toolchain-release/v0"

type toolchainAttestationDocument struct {
	Type          string                        `json:"_type"`
	Subject       []releaseAttestationSubject   `json:"subject"`
	PredicateType string                        `json:"predicateType"`
	Predicate     toolchainAttestationPredicate `json:"predicate"`
}

type toolchainAttestationPredicate struct {
	CatalogDigest    string             `json:"catalogDigest"`
	CatalogMediaType string             `json:"catalogMediaType"`
	FormatVersion    int                `json:"formatVersion"`
	Predecessor      *RuntimeReleaseRef `json:"predecessor"`
	Toolchains       []Toolchain        `json:"toolchains"`
}

type toolchainAttestationExpectation struct {
	release     string
	predecessor *RuntimeReleaseRef
}

func VerifyToolchainCatalog(
	catalogBytes,
	bundleBytes,
	trustedRootBytes []byte,
) (*ToolchainCatalog, error) {
	catalog, err := ParseToolchainCatalog(catalogBytes)
	if err != nil {
		return nil, err
	}
	entity, err := parseReleaseBundle(bundleBytes)
	if err != nil {
		return nil, err
	}
	trustedMaterial, err := parseReleaseTrustedRoot(trustedRootBytes)
	if err != nil {
		return nil, err
	}
	if err := authenticateToolchainCatalog(
		catalogBytes,
		catalog,
		entity,
		trustedMaterial,
		nil,
	); err != nil {
		return nil, err
	}
	catalog.authenticated = true
	return catalog, nil
}

func VerifyToolchainCatalogForRelease(
	catalogBytes,
	bundleBytes,
	trustedRootBytes []byte,
	release string,
	predecessor *RuntimeReleaseRef,
) (*ToolchainCatalog, error) {
	if !runtimeReleaseTagPattern.MatchString(release) {
		return nil, fmt.Errorf("standard-toolchain release tag %q is invalid", release)
	}
	if predecessor != nil {
		if err := ValidateRuntimeReleaseRef(*predecessor); err != nil {
			return nil, err
		}
	}
	catalog, err := ParseToolchainCatalog(catalogBytes)
	if err != nil {
		return nil, err
	}
	entity, err := parseReleaseBundle(bundleBytes)
	if err != nil {
		return nil, err
	}
	trustedMaterial, err := parseReleaseTrustedRoot(trustedRootBytes)
	if err != nil {
		return nil, err
	}
	if err := authenticateToolchainCatalog(
		catalogBytes,
		catalog,
		entity,
		trustedMaterial,
		&toolchainAttestationExpectation{
			release:     release,
			predecessor: predecessor,
		},
	); err != nil {
		return nil, err
	}
	catalog.authenticated = true
	return catalog, nil
}

func authenticateToolchainCatalog(
	catalogBytes []byte,
	catalog *ToolchainCatalog,
	entity verify.SignedEntity,
	trustedMaterial root.TrustedMaterial,
	expectation *toolchainAttestationExpectation,
) error {
	if catalog == nil {
		return errors.New("standard-toolchain catalog is required")
	}
	if entity == nil {
		return errors.New("standard-toolchain attestation bundle is required")
	}
	if trustedMaterial == nil {
		return errors.New("standard-toolchain trusted root is required")
	}

	sanPattern := releaseCertificateSANPattern
	if expectation != nil {
		sanPattern = "^" + regexp.QuoteMeta(
			"https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/tags/"+
				expectation.release,
		) + "$"
	}
	catalogHash := sha256.Sum256(catalogBytes)
	raw, err := verifyReleasePayload(
		entity,
		trustedMaterial,
		catalogHash[:],
		sanPattern,
	)
	if err != nil {
		return fmt.Errorf("verify standard-toolchain attestation: %w", err)
	}
	return validateToolchainAttestation(raw, catalogHash, catalog, expectation)
}

func validateToolchainAttestation(
	raw []byte,
	catalogHash [sha256.Size]byte,
	catalog *ToolchainCatalog,
	expectation *toolchainAttestationExpectation,
) error {
	var document toolchainAttestationDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode standard-toolchain attestation payload: %w", err)
	}
	if err := ensureEOF(decoder, "standard-toolchain attestation payload"); err != nil {
		return err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return fmt.Errorf("canonicalize standard-toolchain attestation payload: %w", err)
	}
	complete, err := canonicalToolchainAttestationDocument(document)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, complete) {
		return errors.New("standard-toolchain attestation payload does not match the complete closed v0 shape")
	}
	if document.Type != RuntimeAttestationType {
		return fmt.Errorf(
			"standard-toolchain attestation _type = %q, want %q",
			document.Type,
			RuntimeAttestationType,
		)
	}
	if document.PredicateType != ToolchainAttestationPredicateType {
		return fmt.Errorf(
			"standard-toolchain attestation predicateType = %q, want %q",
			document.PredicateType,
			ToolchainAttestationPredicateType,
		)
	}
	if err := validateToolchainAttestationSubjects(document.Subject, catalogHash, catalog); err != nil {
		return err
	}
	return validateToolchainAttestationPredicate(
		document.Predicate,
		catalogHash,
		catalog,
		expectation,
	)
}

func validateToolchainAttestationSubjects(
	subjects []releaseAttestationSubject,
	catalogHash [sha256.Size]byte,
	catalog *ToolchainCatalog,
) error {
	expected := make(map[string]string, len(catalog.toolchains)+1)
	expected["catalog"] = hex.EncodeToString(catalogHash[:])
	for _, toolchain := range catalog.toolchains {
		digest := toolchain.ToolchainClosure.Digest
		hexDigest := digest[len("sha256:"):]
		expected["toolchain/sha256/"+hexDigest] = hexDigest
	}
	if len(subjects) != len(expected) {
		return fmt.Errorf(
			"standard-toolchain attestation subject count = %d, want %d",
			len(subjects),
			len(expected),
		)
	}
	seen := make(map[string]struct{}, len(subjects))
	for _, subject := range subjects {
		if _, ok := seen[subject.Name]; ok {
			return fmt.Errorf(
				"standard-toolchain attestation contains duplicate subject %q",
				subject.Name,
			)
		}
		seen[subject.Name] = struct{}{}
		want, ok := expected[subject.Name]
		if !ok {
			return fmt.Errorf(
				"standard-toolchain attestation contains unexpected subject %q",
				subject.Name,
			)
		}
		if len(subject.Digest) != 1 || subject.Digest["sha256"] != want {
			return fmt.Errorf(
				"standard-toolchain attestation subject %q does not have the exact SHA-256 digest",
				subject.Name,
			)
		}
	}
	return nil
}

func validateToolchainAttestationPredicate(
	predicate toolchainAttestationPredicate,
	catalogHash [sha256.Size]byte,
	catalog *ToolchainCatalog,
	expectation *toolchainAttestationExpectation,
) error {
	if predicate.FormatVersion != ToolchainCatalogFormatVersion {
		return fmt.Errorf(
			"standard-toolchain attestation predicate formatVersion = %d, want %d",
			predicate.FormatVersion,
			ToolchainCatalogFormatVersion,
		)
	}
	if predicate.CatalogMediaType != ToolchainCatalogMediaType {
		return fmt.Errorf(
			"standard-toolchain attestation predicate catalogMediaType = %q, want %q",
			predicate.CatalogMediaType,
			ToolchainCatalogMediaType,
		)
	}
	wantDigest := "sha256:" + hex.EncodeToString(catalogHash[:])
	if predicate.CatalogDigest != wantDigest {
		return fmt.Errorf(
			"standard-toolchain attestation predicate catalogDigest = %q, want %q",
			predicate.CatalogDigest,
			wantDigest,
		)
	}
	if predicate.Predecessor != nil {
		if err := ValidateRuntimeReleaseRef(*predicate.Predecessor); err != nil {
			return fmt.Errorf("standard-toolchain attestation predecessor: %w", err)
		}
	}
	if expectation != nil && !runtimeReleaseRefsEqual(
		predicate.Predecessor,
		expectation.predecessor,
	) {
		return errors.New(
			"standard-toolchain attestation predecessor does not exact-match release lineage",
		)
	}
	toolchainsBytes, err := canonicalToolchains(predicate.Toolchains)
	if err != nil {
		return err
	}
	if !bytes.Equal(toolchainsBytes, catalog.toolchainsBytes) {
		return errors.New("standard-toolchain attestation predicate toolchains do not exact-match catalog")
	}
	return nil
}

func canonicalToolchainAttestationDocument(
	document toolchainAttestationDocument,
) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode standard-toolchain attestation payload: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize standard-toolchain attestation payload: %w", err)
	}
	return canonical, nil
}
