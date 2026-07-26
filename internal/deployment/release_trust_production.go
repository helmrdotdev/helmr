//go:build !helmrdevtrust

package deployment

import "github.com/sigstore/sigstore-go/pkg/verify"

func compiledReleaseTrustPolicy() (ReleaseTrustPolicy, error) {
	return ReleaseTrustPolicy{
		Mode:       "production",
		Issuer:     releaseAttestationIssuer,
		SANPattern: releaseCertificateSANPattern,
	}, nil
}

func compiledReleaseCertificateIdentity(
	sanPattern string,
) (verify.CertificateIdentity, error) {
	return verify.NewShortCertificateIdentity(
		releaseAttestationIssuer,
		"",
		"",
		sanPattern,
	)
}
