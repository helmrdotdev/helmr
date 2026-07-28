package control

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func controlBuildPolicy(t *testing.T) *deployment.BuildPolicy {
	t.Helper()
	digest := func(character string) string {
		return "sha256:" + strings.Repeat(character, 64)
	}
	raw := `{"architecture":"x86_64","denies":{"digests":[],"selectors":[]},"descriptorSchemaVersion":0,"fixtureSet":"helmr.platform.fixtures.v0","formatVersion":0,"managers":[{"adapterVersion":"helmr.manager.v0","allowedOrigin":"https://github.com/oven-sh/bun/releases/","allowedRedirectHosts":["api.github.com","github.com","objects.githubusercontent.com"],"domain":{"major":1,"minimum":"1.3.10"},"metadataOrigin":"https://api.github.com/repos/oven-sh/bun/releases/tags/","name":"bun"},{"adapterVersion":"helmr.manager.v0","allowedOrigin":"https://registry.npmjs.org/npm/","allowedRedirectHosts":["registry.npmjs.org"],"domain":{"major":11,"minimum":"11.4.2"},"metadataOrigin":"https://registry.npmjs.org/npm/","name":"npm"},{"adapterVersion":"helmr.manager.v0","allowedOrigin":"https://registry.npmjs.org/pnpm/","allowedRedirectHosts":["registry.npmjs.org"],"domain":{"major":11,"minimum":"11.1.0"},"metadataOrigin":"https://registry.npmjs.org/pnpm/","name":"pnpm"}],"node":{"adapterVersion":"helmr.runtime.v0","allowedOrigin":"https://nodejs.org/dist/","allowedRedirectHosts":["nodejs.org"],"domains":[{"major":22,"minimum":"22.18.0"},{"major":24,"minimum":"24.3.0"}],"releaseKeyFingerprints":["00112233445566778899AABBCCDDEEFF00112233"],"releaseKeyring":"AQ=="},"runtime":{"configEvaluatorDigest":"` + digest("1") + `","harness":{"digest":"` + digest("2") + `","mediaType":"application/vnd.helmr.platform-tree.v0+tar","sizeBytes":4096}},"toolchain":{"base":{"digest":"` + digest("3") + `","mediaType":"application/vnd.helmr.platform-tree.v0+tar","sizeBytes":4096}}}`
	policy, err := deployment.ParseBuildPolicy([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}
