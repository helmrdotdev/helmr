package deployment

import (
	"encoding/json"
	"errors"
	"fmt"

	sigbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	ReleaseBundleMediaType       = "application/vnd.dev.sigstore.bundle.v0.3+json"
	releaseAttestationIssuer     = "https://token.actions.githubusercontent.com"
	releaseCertificateSANPattern = `^https://github\.com/helmrdotdev/helmr/\.github/workflows/release\.yaml@refs/tags/v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:(?:0|[1-9][0-9]*)|(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))(?:\.(?:(?:0|[1-9][0-9]*)|(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)))*)?$`
	maxReleaseBundleBytes        = maxProgramFileSizeBytes
	maxReleaseTrustedRootBytes   = maxProgramFileSizeBytes
)

func parseReleaseBundle(raw []byte) (*sigbundle.Bundle, error) {
	if len(raw) == 0 || int64(len(raw)) > maxReleaseBundleBytes {
		return nil, fmt.Errorf(
			"release attestation bundle size is outside [1,%d]",
			maxReleaseBundleBytes,
		)
	}
	var entity sigbundle.Bundle
	if err := json.Unmarshal(raw, &entity); err != nil {
		return nil, fmt.Errorf("decode release attestation bundle: %w", err)
	}
	if entity.MediaType != ReleaseBundleMediaType {
		return nil, fmt.Errorf(
			"release attestation bundle mediaType = %q, want %q",
			entity.MediaType,
			ReleaseBundleMediaType,
		)
	}
	if _, err := entity.Envelope(); err != nil {
		return nil, fmt.Errorf("release attestation bundle does not contain a DSSE envelope: %w", err)
	}
	return &entity, nil
}

func parseReleaseTrustedRoot(raw []byte) (root.TrustedMaterial, error) {
	if len(raw) == 0 || int64(len(raw)) > maxReleaseTrustedRootBytes {
		return nil, fmt.Errorf(
			"release trusted root size is outside [1,%d]",
			maxReleaseTrustedRootBytes,
		)
	}
	trustedRoot, err := root.NewTrustedRootFromJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode release trusted root: %w", err)
	}
	return trustedRoot, nil
}

func verifyReleasePayload(
	entity verify.SignedEntity,
	trustedMaterial root.TrustedMaterial,
	artifactDigest []byte,
	sanPattern string,
) ([]byte, error) {
	if entity == nil {
		return nil, errors.New("release attestation bundle is required")
	}
	if trustedMaterial == nil {
		return nil, errors.New("release trusted root is required")
	}
	identity, err := verify.NewShortCertificateIdentity(
		releaseAttestationIssuer,
		"",
		"",
		sanPattern,
	)
	if err != nil {
		return nil, fmt.Errorf("configure release attestation identity: %w", err)
	}
	verifier, err := verify.NewVerifier(
		trustedMaterial,
		verify.WithTransparencyLog(1),
		verify.WithIntegratedTimestamps(1),
	)
	if err != nil {
		return nil, fmt.Errorf("configure release attestation verifier: %w", err)
	}
	if _, err := verifier.Verify(
		entity,
		verify.NewPolicy(
			verify.WithArtifactDigest("sha256", artifactDigest),
			verify.WithCertificateIdentity(identity),
		),
	); err != nil {
		return nil, fmt.Errorf("verify release attestation: %w", err)
	}
	signature, err := entity.SignatureContent()
	if err != nil {
		return nil, fmt.Errorf("read verified release attestation: %w", err)
	}
	envelope := signature.EnvelopeContent()
	if envelope == nil {
		return nil, errors.New("verified release attestation is not a DSSE envelope")
	}
	raw, err := envelope.RawEnvelope().DecodeB64Payload()
	if err != nil {
		return nil, fmt.Errorf("decode verified release attestation payload: %w", err)
	}
	return raw, nil
}
