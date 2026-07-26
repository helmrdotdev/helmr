//go:build helmrdevtrust

package deployment

import "testing"

func TestCompiledDevelopmentReleaseTrustPolicy(t *testing.T) {
	policy, err := CompiledReleaseTrustPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != "development" ||
		policy.Issuer != releaseAttestationIssuer ||
		policy.SAN != devReleaseCertificateSAN ||
		policy.SANPattern != "" ||
		policy.SourceRepositoryDigest != devReleaseSourceRepositoryDigest {
		t.Fatalf("unexpected development release trust policy: %#v", policy)
	}
	if _, err := compiledReleaseCertificateIdentity("ignored"); err != nil {
		t.Fatal(err)
	}
	identity, err := compiledReleaseCertificateIdentity(
		releaseCertificateSANPattern,
	)
	if err != nil {
		t.Fatal(err)
	}
	if identity.SubjectAlternativeName.SubjectAlternativeName != policy.SAN ||
		identity.Extensions.SourceRepositoryDigest != policy.SourceRepositoryDigest {
		t.Fatalf("development identity is not exact-bound: %#v", identity)
	}
}

func TestDevelopmentReleaseTrustFailsClosed(t *testing.T) {
	originalSAN := devReleaseCertificateSAN
	originalDigest := devReleaseSourceRepositoryDigest
	shortDigest := originalDigest
	if len(shortDigest) > 12 {
		shortDigest = shortDigest[:12]
	}
	t.Cleanup(func() {
		devReleaseCertificateSAN = originalSAN
		devReleaseSourceRepositoryDigest = originalDigest
	})

	tests := map[string]struct {
		san    string
		digest string
	}{
		"production tag identity": {
			san:    "https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/tags/v1.2.3",
			digest: originalDigest,
		},
		"other workflow": {
			san:    "https://github.com/helmrdotdev/helmr/.github/workflows/dev-runtime.yaml@refs/heads/feature",
			digest: originalDigest,
		},
		"empty digest": {
			san:    originalSAN,
			digest: "",
		},
		"short digest": {
			san:    originalSAN,
			digest: shortDigest,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			devReleaseCertificateSAN = test.san
			devReleaseSourceRepositoryDigest = test.digest
			if _, err := CompiledReleaseTrustPolicy(); err == nil {
				t.Fatal("development release trust accepted invalid input")
			}
		})
	}
}
