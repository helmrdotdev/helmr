//go:build !helmrdevtrust

package deployment

import "testing"

func TestCompiledProductionReleaseTrustPolicy(t *testing.T) {
	policy, err := CompiledReleaseTrustPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != "production" ||
		policy.Issuer != releaseAttestationIssuer ||
		policy.SAN != "" ||
		policy.SANPattern != releaseCertificateSANPattern ||
		policy.SourceRepositoryDigest != "" {
		t.Fatalf("unexpected production release trust policy: %#v", policy)
	}
}
