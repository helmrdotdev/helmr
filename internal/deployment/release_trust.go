package deployment

// ReleaseTrustPolicy is the closed, compiled identity accepted by release
// attestation verification. It is exposed so release and image build pipelines
// can positively assert the policy compiled into their verifier.
type ReleaseTrustPolicy struct {
	Mode                   string `json:"mode"`
	Issuer                 string `json:"issuer"`
	SAN                    string `json:"san,omitempty"`
	SANPattern             string `json:"sanPattern,omitempty"`
	SourceRepositoryDigest string `json:"sourceRepositoryDigest,omitempty"`
}

// CompiledReleaseTrustPolicy returns the release identity compiled into this
// binary. Development builds fail closed until their exact workflow identity
// and source commit have been injected by the build pipeline.
func CompiledReleaseTrustPolicy() (ReleaseTrustPolicy, error) {
	return compiledReleaseTrustPolicy()
}
