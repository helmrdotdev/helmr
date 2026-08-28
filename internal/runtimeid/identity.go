package runtimeid

import (
	"fmt"

	"github.com/helmrdotdev/helmr/capacity"
)

type Selector struct {
	ID                        string                       `json:"id"`
	Arch                      string                       `json:"arch"`
	Contract                  string                       `json:"contract"`
	VMRuntimeDescriptorDigest string                       `json:"vm_runtime_descriptor_digest"`
	FirecrackerDigest         string                       `json:"firecracker_digest"`
	FirecrackerVersion        string                       `json:"firecracker_version"`
	SnapshotFormatVersion     string                       `json:"snapshot_format_version"`
	HostKernelRelease         string                       `json:"host_kernel_release"`
	CPUTemplate               capacity.CPUTemplateSelector `json:"cpu_template"`
	KernelDigest              string                       `json:"kernel_digest"`
	InitramfsDigest           string                       `json:"initramfs_digest"`
	RootfsDigest              string                       `json:"rootfs_digest"`
}

const Contract = capacity.RuntimeContract

func ArchitectureFromGo(value string) (string, error) {
	if value == "amd64" {
		return "x86_64", nil
	}
	return "", fmt.Errorf("unsupported Go architecture %q", value)
}

func Digest(runtime Selector) (string, error) {
	return (capacity.RuntimeProfile{
		Arch:                      runtime.Arch,
		Contract:                  runtime.Contract,
		VMRuntimeDescriptorDigest: runtime.VMRuntimeDescriptorDigest,
		FirecrackerDigest:         runtime.FirecrackerDigest,
		FirecrackerVersion:        runtime.FirecrackerVersion,
		SnapshotFormatVersion:     runtime.SnapshotFormatVersion,
		HostKernelRelease:         runtime.HostKernelRelease,
		CPUTemplate:               runtime.CPUTemplate,
		KernelDigest:              runtime.KernelDigest,
		InitramfsDigest:           runtime.InitramfsDigest,
		RootfsDigest:              runtime.RootfsDigest,
	}).ExpectedID()
}
