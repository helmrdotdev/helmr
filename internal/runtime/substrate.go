package runtime

import "github.com/helmrdotdev/helmr/internal/substrate"

func substrateKey(sandboxDigest string, sandboxFormat string, imageDigest string, rootfsDigest string, runtimeABI string, guestdABI string, adapterABI string, workspaceMountPath string) string {
	key, err := substrate.CacheKey(substrate.Source{
		SandboxArtifactDigest: sandboxDigest,
		SandboxArtifactFormat: sandboxFormat,
		ImageDigest:           imageDigest,
		RootfsDigest:          rootfsDigest,
		RuntimeABI:            runtimeABI,
		GuestdABI:             guestdABI,
		AdapterABI:            adapterABI,
		WorkspaceMountPath:    workspaceMountPath,
	})
	if err != nil {
		return ""
	}
	return key
}
