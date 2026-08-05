package runtimeid

import (
	"encoding/json"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type Selector struct {
	ID              string `json:"id"`
	Arch            string `json:"arch"`
	Contract        string `json:"contract"`
	KernelDigest    string `json:"kernel_digest"`
	InitramfsDigest string `json:"initramfs_digest"`
	RootfsDigest    string `json:"rootfs_digest"`
}

const (
	digestDomain = "helmr.vm-runtime-identity.v0"
	Contract     = "helmr.vm-runtime.v0"
)

func ArchitectureFromGo(value string) (string, error) {
	if value == "amd64" {
		return "x86_64", nil
	}
	return "", fmt.Errorf("unsupported Go architecture %q", value)
}

func Digest(runtime Selector) (string, error) {
	payload, err := json.Marshal(struct {
		Domain          string `json:"domain"`
		Backend         string `json:"backend"`
		Arch            string `json:"arch"`
		Contract        string `json:"contract"`
		KernelDigest    string `json:"kernel_digest"`
		InitramfsDigest string `json:"initramfs_digest"`
		RootfsDigest    string `json:"rootfs_digest"`
	}{
		Domain:          digestDomain,
		Backend:         "firecracker",
		Arch:            runtime.Arch,
		Contract:        runtime.Contract,
		KernelDigest:    runtime.KernelDigest,
		InitramfsDigest: runtime.InitramfsDigest,
		RootfsDigest:    runtime.RootfsDigest,
	})
	if err != nil {
		return "", err
	}
	return sha256sum.DigestBytes(payload), nil
}
