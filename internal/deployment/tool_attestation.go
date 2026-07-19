package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	ToolAttestationPredicateType = "https://helmr.dev/attestations/dependency-tool-release/v0"
)

type toolAttestationDocument struct {
	Type          string                      `json:"_type"`
	Subject       []releaseAttestationSubject `json:"subject"`
	PredicateType string                      `json:"predicateType"`
	Predicate     toolAttestationPredicate    `json:"predicate"`
}

type toolAttestationPredicate struct {
	Corpora       []toolCorpusDocument `json:"corpora"`
	FormatVersion int                  `json:"formatVersion"`
	Objects       []ToolObject         `json:"objects"`
	Registry      ToolObject           `json:"registry"`
}

func VerifyToolRegistry(
	registryBytes,
	bundleBytes,
	trustedRootBytes []byte,
) (*ToolRegistry, error) {
	return verifyToolRegistry(
		registryBytes,
		bundleBytes,
		trustedRootBytes,
		releaseCertificateSANPattern,
	)
}

func VerifyToolRegistryForRelease(
	registryBytes,
	bundleBytes,
	trustedRootBytes []byte,
	release string,
) (*ToolRegistry, error) {
	if !runtimeReleaseTagPattern.MatchString(release) {
		return nil, fmt.Errorf("dependency tool release tag %q is invalid", release)
	}
	return verifyToolRegistry(
		registryBytes,
		bundleBytes,
		trustedRootBytes,
		"^"+regexp.QuoteMeta(
			"https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/tags/"+
				release,
		)+"$",
	)
}

func verifyToolRegistry(
	registryBytes,
	bundleBytes,
	trustedRootBytes []byte,
	sanPattern string,
) (*ToolRegistry, error) {
	registry, err := ParseToolRegistry(registryBytes)
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
	if err := authenticateToolRegistry(
		registryBytes,
		registry,
		entity,
		trustedMaterial,
		sanPattern,
	); err != nil {
		return nil, err
	}
	registry.authenticated = true
	return registry, nil
}

func authenticateToolRegistry(
	registryBytes []byte,
	registry *ToolRegistry,
	entity verify.SignedEntity,
	trustedMaterial root.TrustedMaterial,
	sanPattern string,
) error {
	if registry == nil {
		return errors.New("dependency tool registry is required")
	}
	registryHash := sha256.Sum256(registryBytes)
	raw, err := verifyReleasePayload(
		entity,
		trustedMaterial,
		registryHash[:],
		sanPattern,
	)
	if err != nil {
		return fmt.Errorf("verify dependency tool attestation: %w", err)
	}
	return validateToolAttestation(raw, registryHash, registry)
}

func validateToolAttestation(
	raw []byte,
	registryHash [sha256.Size]byte,
	registry *ToolRegistry,
) error {
	var document toolAttestationDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode dependency tool attestation payload: %w", err)
	}
	if err := ensureEOF(decoder, "dependency tool attestation payload"); err != nil {
		return err
	}
	complete, err := canonicalToolAttestationDocument(document)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, complete) {
		return errors.New("dependency tool attestation payload does not match the complete closed v0 shape")
	}
	if document.Type != RuntimeAttestationType {
		return fmt.Errorf(
			"dependency tool attestation _type = %q, want %q",
			document.Type,
			RuntimeAttestationType,
		)
	}
	if document.PredicateType != ToolAttestationPredicateType {
		return fmt.Errorf(
			"dependency tool attestation predicateType = %q, want %q",
			document.PredicateType,
			ToolAttestationPredicateType,
		)
	}
	expected, err := toolAttestationForRegistry(registry)
	if err != nil {
		return err
	}
	if expected.Registry.Digest != "sha256:"+hex.EncodeToString(registryHash[:]) {
		return errors.New("dependency tool registry bytes do not match parsed identity")
	}
	if err := validateToolAttestationSubjects(document.Subject, expected); err != nil {
		return err
	}
	expectedBytes, err := canonicalToolAttestationPredicate(expected)
	if err != nil {
		return err
	}
	actualBytes, err := canonicalToolAttestationPredicate(document.Predicate)
	if err != nil {
		return err
	}
	if !bytes.Equal(actualBytes, expectedBytes) {
		return errors.New("dependency tool attestation predicate does not exact-match the registry closure")
	}
	return nil
}

func toolAttestationForRegistry(
	registry *ToolRegistry,
) (toolAttestationPredicate, error) {
	if registry == nil || len(registry.raw) == 0 || !validToolDigest(registry.digest) {
		return toolAttestationPredicate{}, errors.New("dependency tool registry is required")
	}
	architectures := toolRegistryArchitectures(registry)
	corpora := make([]toolCorpusDocument, 0, len(architectures))
	for _, architecture := range architectures {
		corpus, err := toolCorpusForArchitecture(registry, architecture)
		if err != nil {
			return toolAttestationPredicate{}, err
		}
		corpora = append(corpora, corpus)
	}
	objects, _, err := normalizeToolObjects(toolRegistryObjects(toolRegistryDocument{
		FormatVersion: ToolsetFormatVersion,
		Managers:      registry.managers,
		Toolchains:    registry.toolchains,
		Toolsets:      registry.toolsets,
	}))
	if err != nil {
		return toolAttestationPredicate{}, err
	}
	return toolAttestationPredicate{
		Corpora:       corpora,
		FormatVersion: ToolsetFormatVersion,
		Objects:       objects,
		Registry: ToolObject{
			Digest:    registry.digest,
			MediaType: ToolRegistryMediaType,
			SizeBytes: int64(len(registry.raw)),
		},
	}, nil
}

func validateToolAttestationSubjects(
	subjects []releaseAttestationSubject,
	predicate toolAttestationPredicate,
) error {
	expected := make(map[string]string, len(predicate.Corpora)+len(predicate.Objects)+1)
	expected["registry"] = predicate.Registry.Digest[len("sha256:"):]
	for _, corpus := range predicate.Corpora {
		raw, err := canonicalToolCorpusDocument(corpus)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(raw)
		expected["corpus/"+string(corpus.Architecture)] = hex.EncodeToString(hash[:])
	}
	for _, object := range predicate.Objects {
		hexDigest := object.Digest[len("sha256:"):]
		expected["object/sha256/"+hexDigest] = hexDigest
	}
	if len(subjects) != len(expected) {
		return fmt.Errorf(
			"dependency tool attestation subject count = %d, want %d",
			len(subjects),
			len(expected),
		)
	}
	seen := make(map[string]struct{}, len(subjects))
	for _, subject := range subjects {
		if _, exists := seen[subject.Name]; exists {
			return fmt.Errorf(
				"dependency tool attestation contains duplicate subject %q",
				subject.Name,
			)
		}
		seen[subject.Name] = struct{}{}
		want, ok := expected[subject.Name]
		if !ok {
			return fmt.Errorf(
				"dependency tool attestation contains unexpected subject %q",
				subject.Name,
			)
		}
		if len(subject.Digest) != 1 || subject.Digest["sha256"] != want {
			return fmt.Errorf(
				"dependency tool attestation subject %q does not have the exact SHA-256 digest",
				subject.Name,
			)
		}
	}
	return nil
}

func toolRegistryArchitectures(registry *ToolRegistry) []RuntimeArchitecture {
	values := make(map[RuntimeArchitecture]struct{})
	for _, manager := range registry.managers {
		values[manager.Architecture] = struct{}{}
	}
	for _, toolchain := range registry.toolchains {
		values[toolchain.Architecture] = struct{}{}
	}
	for _, toolset := range registry.toolsets {
		values[toolset.Architecture] = struct{}{}
	}
	architectures := make([]RuntimeArchitecture, 0, len(values))
	for architecture := range values {
		architectures = append(architectures, architecture)
	}
	sort.Slice(architectures, func(left, right int) bool {
		return architectures[left] < architectures[right]
	})
	return architectures
}

func canonicalToolAttestationDocument(
	document toolAttestationDocument,
) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode dependency tool attestation payload: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize dependency tool attestation payload: %w", err)
	}
	return canonical, nil
}

func canonicalToolAttestationPredicate(
	predicate toolAttestationPredicate,
) ([]byte, error) {
	raw, err := json.Marshal(predicate)
	if err != nil {
		return nil, fmt.Errorf("encode dependency tool attestation predicate: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize dependency tool attestation predicate: %w", err)
	}
	return canonical, nil
}
