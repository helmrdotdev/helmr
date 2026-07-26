//go:build helmrdevtrust

package deployment

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/verify"
)

const devReleaseCertificateSANPrefix = "https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/heads/"

var (
	devReleaseCertificateSAN         string
	devReleaseSourceRepositoryDigest string
	devReleaseSourceDigestPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	devReleaseRefPattern             = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._/-]*$`)
)

func compiledReleaseTrustPolicy() (ReleaseTrustPolicy, error) {
	ref, ok := strings.CutPrefix(
		devReleaseCertificateSAN,
		devReleaseCertificateSANPrefix,
	)
	if !ok || !devReleaseRefPattern.MatchString(ref) ||
		strings.Contains(ref, "..") ||
		strings.Contains(ref, "//") ||
		strings.Contains(ref, "@{") ||
		strings.HasSuffix(ref, "/") {
		return ReleaseTrustPolicy{}, errors.New(
			"development release trust requires an exact Helmr release workflow branch identity",
		)
	}
	if !devReleaseSourceDigestPattern.MatchString(
		devReleaseSourceRepositoryDigest,
	) {
		return ReleaseTrustPolicy{}, errors.New(
			"development release trust requires an exact 40-character lowercase source commit",
		)
	}
	return ReleaseTrustPolicy{
		Mode:                   "development",
		Issuer:                 releaseAttestationIssuer,
		SAN:                    devReleaseCertificateSAN,
		SourceRepositoryDigest: devReleaseSourceRepositoryDigest,
	}, nil
}

func compiledReleaseCertificateIdentity(
	_ string,
) (verify.CertificateIdentity, error) {
	policy, err := compiledReleaseTrustPolicy()
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf(
			"configure development release trust: %w",
			err,
		)
	}
	identity, err := verify.NewShortCertificateIdentity(
		policy.Issuer,
		"",
		policy.SAN,
		"",
	)
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	identity.Extensions.SourceRepositoryDigest = policy.SourceRepositoryDigest
	return identity, nil
}
