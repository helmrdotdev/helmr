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
	sigbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	RuntimeBundleMediaType              = "application/vnd.dev.sigstore.bundle.v0.3+json"
	RuntimeAttestationType              = "https://in-toto.io/Statement/v1"
	RuntimeAttestationPredicateType     = "https://helmr.dev/attestations/runtime-release/v0"
	RuntimeAttestationFormatVersion     = 0
	runtimeAttestationIssuer            = "https://token.actions.githubusercontent.com"
	maxRuntimeBundleBytes               = maxProgramFileSizeBytes
	maxRuntimeTrustedRootBytes          = maxProgramFileSizeBytes
	runtimeReleaseCertificateSANPattern = `^https://github\.com/helmrdotdev/helmr/\.github/workflows/release\.yaml@refs/tags/v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:(?:0|[1-9][0-9]*)|(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))(?:\.(?:(?:0|[1-9][0-9]*)|(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)))*)?$`
)

type runtimeAttestationDocument struct {
	Type          string                      `json:"_type"`
	Subject       []runtimeAttestationSubject `json:"subject"`
	PredicateType string                      `json:"predicateType"`
	Predicate     runtimeAttestationPredicate `json:"predicate"`
}

type runtimeAttestationSubject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type runtimeAttestationPredicate struct {
	CatalogDigest    string              `json:"catalogDigest"`
	CatalogMediaType string              `json:"catalogMediaType"`
	FormatVersion    int                 `json:"formatVersion"`
	Predecessor      *RuntimeReleaseRef  `json:"predecessor"`
	Runtimes         []RuntimeDescriptor `json:"runtimes"`
}

type runtimeAttestationExpectation struct {
	release     string
	predecessor *RuntimeReleaseRef
}

// VerifyRuntimeCatalog performs offline verification against explicit,
// caller-pinned trusted-root bytes. Operational code should use
// LoadRuntimeCatalog, which supplies the release-owned image inputs.
func VerifyRuntimeCatalog(
	catalogBytes,
	bundleBytes,
	trustedRootBytes []byte,
) (*RuntimeCatalog, error) {
	catalog, err := ParseRuntimeCatalog(catalogBytes)
	if err != nil {
		return nil, err
	}
	entity, err := parseRuntimeBundle(bundleBytes)
	if err != nil {
		return nil, err
	}
	trustedMaterial, err := parseRuntimeTrustedRoot(trustedRootBytes)
	if err != nil {
		return nil, err
	}
	if err := authenticateRuntimeCatalog(
		catalogBytes,
		catalog,
		entity,
		trustedMaterial,
	); err != nil {
		return nil, err
	}
	catalog.authenticated = true
	return catalog, nil
}

func VerifyRuntimeCatalogForRelease(
	catalogBytes,
	bundleBytes,
	trustedRootBytes []byte,
	release string,
	predecessor *RuntimeReleaseRef,
) (*RuntimeCatalog, error) {
	if !runtimeReleaseTagPattern.MatchString(release) {
		return nil, fmt.Errorf("runtime release tag %q is invalid", release)
	}
	if predecessor != nil {
		if err := ValidateRuntimeReleaseRef(*predecessor); err != nil {
			return nil, err
		}
	}
	catalog, err := ParseRuntimeCatalog(catalogBytes)
	if err != nil {
		return nil, err
	}
	entity, err := parseRuntimeBundle(bundleBytes)
	if err != nil {
		return nil, err
	}
	trustedMaterial, err := parseRuntimeTrustedRoot(trustedRootBytes)
	if err != nil {
		return nil, err
	}
	if err := authenticateRuntimeCatalogWithExpectation(
		catalogBytes,
		catalog,
		entity,
		trustedMaterial,
		&runtimeAttestationExpectation{
			release:     release,
			predecessor: predecessor,
		},
	); err != nil {
		return nil, err
	}
	catalog.authenticated = true
	return catalog, nil
}

func parseRuntimeBundle(raw []byte) (*sigbundle.Bundle, error) {
	if len(raw) == 0 || int64(len(raw)) > maxRuntimeBundleBytes {
		return nil, fmt.Errorf(
			"runtime attestation bundle size is outside [1,%d]",
			maxRuntimeBundleBytes,
		)
	}
	var entity sigbundle.Bundle
	if err := json.Unmarshal(raw, &entity); err != nil {
		return nil, fmt.Errorf("decode runtime attestation bundle: %w", err)
	}
	if entity.MediaType != RuntimeBundleMediaType {
		return nil, fmt.Errorf(
			"runtime attestation bundle mediaType = %q, want %q",
			entity.MediaType,
			RuntimeBundleMediaType,
		)
	}
	if _, err := entity.Envelope(); err != nil {
		return nil, fmt.Errorf("runtime attestation bundle does not contain a DSSE envelope: %w", err)
	}
	return &entity, nil
}

func parseRuntimeTrustedRoot(raw []byte) (root.TrustedMaterial, error) {
	if len(raw) == 0 || int64(len(raw)) > maxRuntimeTrustedRootBytes {
		return nil, fmt.Errorf(
			"runtime trusted root size is outside [1,%d]",
			maxRuntimeTrustedRootBytes,
		)
	}
	trustedRoot, err := root.NewTrustedRootFromJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode runtime trusted root: %w", err)
	}
	return trustedRoot, nil
}

func authenticateRuntimeCatalog(
	catalogBytes []byte,
	catalog *RuntimeCatalog,
	entity verify.SignedEntity,
	trustedMaterial root.TrustedMaterial,
) error {
	return authenticateRuntimeCatalogWithExpectation(
		catalogBytes,
		catalog,
		entity,
		trustedMaterial,
		nil,
	)
}

func authenticateRuntimeCatalogWithExpectation(
	catalogBytes []byte,
	catalog *RuntimeCatalog,
	entity verify.SignedEntity,
	trustedMaterial root.TrustedMaterial,
	expectation *runtimeAttestationExpectation,
) error {
	if catalog == nil {
		return errors.New("runtime catalog is required")
	}
	if entity == nil {
		return errors.New("runtime attestation bundle is required")
	}
	if trustedMaterial == nil {
		return errors.New("runtime trusted root is required")
	}

	sanPattern := runtimeReleaseCertificateSANPattern
	if expectation != nil {
		sanPattern = "^" + regexp.QuoteMeta(
			"https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/tags/"+
				expectation.release,
		) + "$"
	}
	identity, err := verify.NewShortCertificateIdentity(
		runtimeAttestationIssuer,
		"",
		"",
		sanPattern,
	)
	if err != nil {
		return fmt.Errorf("configure runtime attestation identity: %w", err)
	}
	verifier, err := verify.NewVerifier(
		trustedMaterial,
		verify.WithTransparencyLog(1),
		verify.WithIntegratedTimestamps(1),
	)
	if err != nil {
		return fmt.Errorf("configure runtime attestation verifier: %w", err)
	}

	catalogHash := sha256.Sum256(catalogBytes)
	if _, err := verifier.Verify(
		entity,
		verify.NewPolicy(
			verify.WithArtifactDigest("sha256", catalogHash[:]),
			verify.WithCertificateIdentity(identity),
		),
	); err != nil {
		return fmt.Errorf("verify runtime attestation: %w", err)
	}

	signature, err := entity.SignatureContent()
	if err != nil {
		return fmt.Errorf("read verified runtime attestation: %w", err)
	}
	envelope := signature.EnvelopeContent()
	if envelope == nil {
		return errors.New("verified runtime attestation is not a DSSE envelope")
	}
	raw, err := envelope.RawEnvelope().DecodeB64Payload()
	if err != nil {
		return fmt.Errorf("decode verified runtime attestation payload: %w", err)
	}
	if err := validateRuntimeAttestation(
		raw,
		catalogHash,
		catalog,
		expectation,
	); err != nil {
		return err
	}
	return nil
}

func validateRuntimeAttestation(
	raw []byte,
	catalogHash [sha256.Size]byte,
	catalog *RuntimeCatalog,
	expectation *runtimeAttestationExpectation,
) error {
	var document runtimeAttestationDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode runtime attestation payload: %w", err)
	}
	if err := ensureEOF(decoder, "runtime attestation payload"); err != nil {
		return err
	}

	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return fmt.Errorf("canonicalize runtime attestation payload: %w", err)
	}
	complete, err := canonicalRuntimeAttestationDocument(document)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, complete) {
		return errors.New("runtime attestation payload does not match the complete closed v0 shape")
	}

	if document.Type != RuntimeAttestationType {
		return fmt.Errorf(
			"runtime attestation _type = %q, want %q",
			document.Type,
			RuntimeAttestationType,
		)
	}
	if document.PredicateType != RuntimeAttestationPredicateType {
		return fmt.Errorf(
			"runtime attestation predicateType = %q, want %q",
			document.PredicateType,
			RuntimeAttestationPredicateType,
		)
	}
	if err := validateRuntimeAttestationSubjects(
		document.Subject,
		catalogHash,
		catalog,
	); err != nil {
		return err
	}
	return validateRuntimeAttestationPredicate(
		document.Predicate,
		catalogHash,
		catalog,
		expectation,
	)
}

func validateRuntimeAttestationSubjects(
	subjects []runtimeAttestationSubject,
	catalogHash [sha256.Size]byte,
	catalog *RuntimeCatalog,
) error {
	expected := make(map[string]string, len(catalog.runtimes)+1)
	expected["catalog"] = hex.EncodeToString(catalogHash[:])
	for _, descriptor := range catalog.runtimes {
		hexDigest := descriptor.Digest[len("sha256:"):]
		expected["runtime/sha256/"+hexDigest] = hexDigest
	}
	if len(subjects) != len(expected) {
		return fmt.Errorf(
			"runtime attestation subject count = %d, want %d",
			len(subjects),
			len(expected),
		)
	}

	seen := make(map[string]struct{}, len(subjects))
	for _, subject := range subjects {
		if _, ok := seen[subject.Name]; ok {
			return fmt.Errorf("runtime attestation contains duplicate subject %q", subject.Name)
		}
		seen[subject.Name] = struct{}{}
		want, ok := expected[subject.Name]
		if !ok {
			return fmt.Errorf("runtime attestation contains unexpected subject %q", subject.Name)
		}
		if len(subject.Digest) != 1 || subject.Digest["sha256"] != want {
			return fmt.Errorf(
				"runtime attestation subject %q does not have the exact SHA-256 digest",
				subject.Name,
			)
		}
	}
	return nil
}

func validateRuntimeAttestationPredicate(
	predicate runtimeAttestationPredicate,
	catalogHash [sha256.Size]byte,
	catalog *RuntimeCatalog,
	expectation *runtimeAttestationExpectation,
) error {
	if predicate.FormatVersion != RuntimeAttestationFormatVersion {
		return fmt.Errorf(
			"runtime attestation predicate formatVersion = %d, want %d",
			predicate.FormatVersion,
			RuntimeAttestationFormatVersion,
		)
	}
	if predicate.CatalogMediaType != RuntimeCatalogMediaType {
		return fmt.Errorf(
			"runtime attestation predicate catalogMediaType = %q, want %q",
			predicate.CatalogMediaType,
			RuntimeCatalogMediaType,
		)
	}
	wantDigest := "sha256:" + hex.EncodeToString(catalogHash[:])
	if predicate.CatalogDigest != wantDigest {
		return fmt.Errorf(
			"runtime attestation predicate catalogDigest = %q, want %q",
			predicate.CatalogDigest,
			wantDigest,
		)
	}
	if predicate.Predecessor != nil {
		if err := ValidateRuntimeReleaseRef(*predicate.Predecessor); err != nil {
			return fmt.Errorf("runtime attestation predecessor: %w", err)
		}
	}
	if expectation != nil && !runtimeReleaseRefsEqual(
		predicate.Predecessor,
		expectation.predecessor,
	) {
		return errors.New(
			"runtime attestation predecessor does not exact-match release lineage",
		)
	}
	runtimesBytes, err := canonicalRuntimeDescriptors(predicate.Runtimes)
	if err != nil {
		return err
	}
	if !bytes.Equal(runtimesBytes, catalog.runtimesBytes) {
		return errors.New("runtime attestation predicate runtimes do not exact-match catalog")
	}
	return nil
}

func runtimeReleaseRefsEqual(first, second *RuntimeReleaseRef) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return *first == *second
}

func canonicalRuntimeAttestationDocument(
	document runtimeAttestationDocument,
) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode runtime attestation payload: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize runtime attestation payload: %w", err)
	}
	return canonical, nil
}
