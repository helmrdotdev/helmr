package identity

import (
	"encoding/json"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type Selector struct {
	ID              string `json:"id"`
	Arch            string `json:"arch"`
	ABI             string `json:"abi"`
	KernelDigest    string `json:"kernel_digest"`
	InitramfsDigest string `json:"initramfs_digest"`
	RootfsDigest    string `json:"rootfs_digest"`
	CNIProfile      string `json:"cni_profile"`
}

const Schema = "helmr.runtime.identity.v0"

func ArchitectureFromGo(value string) (string, error) {
	if value == "amd64" {
		return "x86_64", nil
	}
	return "", fmt.Errorf("unsupported Go architecture %q", value)
}

func Digest(runtime Selector) (string, error) {
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
		Schema:          Schema,
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
