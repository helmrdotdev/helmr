package compute

import (
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type RuntimeSelector struct {
	ID              string `json:"id"`
	Arch            string `json:"arch"`
	ABI             string `json:"abi"`
	KernelDigest    string `json:"kernel_digest"`
	InitramfsDigest string `json:"initramfs_digest"`
	RootfsDigest    string `json:"rootfs_digest"`
	CNIProfile      string `json:"cni_profile"`
}

const RuntimeIdentitySchema = "helmr.runtime.identity.v0"

func RuntimeIdentityDigest(runtime RuntimeSelector) (string, error) {
	payload, err := json.Marshal(struct {
		Schema          string `json:"schema"`
		Backend         string `json:"backend"`
		Arch            string `json:"arch"`
		ABI             string `json:"abi"`
		KernelDigest    string `json:"kernel_digest"`
		InitramfsDigest string `json:"initramfs_digest"`
		RootfsDigest    string `json:"rootfs_digest"`
		CNIProfile      string `json:"cni_profile"`
	}{
		Schema:          RuntimeIdentitySchema,
		Backend:         "firecracker",
		Arch:            runtime.Arch,
		ABI:             runtime.ABI,
		KernelDigest:    runtime.KernelDigest,
		InitramfsDigest: runtime.InitramfsDigest,
		RootfsDigest:    runtime.RootfsDigest,
		CNIProfile:      runtime.CNIProfile,
	})
	if err != nil {
		return "", err
	}
	return sha256sum.DigestBytes(payload), nil
}
