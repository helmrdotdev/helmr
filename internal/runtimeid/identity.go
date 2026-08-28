package runtimeid

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const Contract = "helmr.vm-runtime.v0"

type CPUTemplateKind string

const (
	CPUTemplateNone   CPUTemplateKind = "none"
	CPUTemplateCustom CPUTemplateKind = "custom"
)

type CPUTemplateSelector struct {
	Kind   CPUTemplateKind `json:"kind"`
	Digest string          `json:"digest,omitempty"`
}

type CPUShape struct {
	VCPUCount       int32  `json:"vcpu_count"`
	CPUConfigDigest string `json:"cpu_config_digest"`
}

type Profile struct {
	ID                        string              `json:"id"`
	Arch                      string              `json:"arch"`
	Contract                  string              `json:"contract"`
	VMRuntimeDescriptorDigest string              `json:"vm_runtime_descriptor_digest"`
	FirecrackerDigest         string              `json:"firecracker_digest"`
	FirecrackerVersion        string              `json:"firecracker_version"`
	SnapshotFormatVersion     string              `json:"snapshot_format_version"`
	HostKernelRelease         string              `json:"host_kernel_release"`
	CPUTemplate               CPUTemplateSelector `json:"cpu_template"`
	KernelDigest              string              `json:"kernel_digest"`
	InitramfsDigest           string              `json:"initramfs_digest"`
	RootfsDigest              string              `json:"rootfs_digest"`
}

func (p Profile) ExpectedID() (string, error) {
	if err := p.validateSelector(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Domain                    string              `json:"domain"`
		Backend                   string              `json:"backend"`
		Arch                      string              `json:"arch"`
		Contract                  string              `json:"contract"`
		VMRuntimeDescriptorDigest string              `json:"vm_runtime_descriptor_digest"`
		FirecrackerDigest         string              `json:"firecracker_digest"`
		FirecrackerVersion        string              `json:"firecracker_version"`
		SnapshotFormatVersion     string              `json:"snapshot_format_version"`
		HostKernelRelease         string              `json:"host_kernel_release"`
		CPUTemplate               CPUTemplateSelector `json:"cpu_template"`
		KernelDigest              string              `json:"kernel_digest"`
		InitramfsDigest           string              `json:"initramfs_digest"`
		RootfsDigest              string              `json:"rootfs_digest"`
	}{
		Domain: "helmr.vm-runtime-identity.v0", Backend: "firecracker",
		Arch: p.Arch, Contract: p.Contract,
		VMRuntimeDescriptorDigest: p.VMRuntimeDescriptorDigest,
		FirecrackerDigest:         p.FirecrackerDigest, FirecrackerVersion: p.FirecrackerVersion,
		SnapshotFormatVersion: p.SnapshotFormatVersion, HostKernelRelease: p.HostKernelRelease,
		CPUTemplate: p.CPUTemplate, KernelDigest: p.KernelDigest,
		InitramfsDigest: p.InitramfsDigest, RootfsDigest: p.RootfsDigest,
	})
	if err != nil {
		return "", err
	}
	return sha256sum.DigestBytes(payload), nil
}

func (p Profile) Validate() error {
	expected, err := p.ExpectedID()
	if err != nil {
		return err
	}
	if p.ID != expected {
		return errors.New("runtime.id does not match the canonical runtime selector")
	}
	return nil
}

func (p Profile) validateSelector() error {
	var problems []error
	if p.Arch != "x86_64" || p.Contract != Contract {
		problems = append(problems, errors.New("runtime architecture or contract is not supported"))
	}
	for _, field := range []struct{ name, value string }{
		{name: "vm_runtime_descriptor_digest", value: p.VMRuntimeDescriptorDigest},
		{name: "firecracker_digest", value: p.FirecrackerDigest},
		{name: "kernel_digest", value: p.KernelDigest},
		{name: "initramfs_digest", value: p.InitramfsDigest},
		{name: "rootfs_digest", value: p.RootfsDigest},
	} {
		if !sha256sum.ValidDigest(field.value) {
			problems = append(problems, fmt.Errorf("runtime.%s must be a canonical SHA-256 digest", field.name))
		}
	}
	if !validSemanticVersion(p.FirecrackerVersion) {
		problems = append(problems, errors.New("runtime.firecracker_version must be a canonical semantic version"))
	}
	if !validSemanticVersion(p.SnapshotFormatVersion) {
		problems = append(problems, errors.New("runtime.snapshot_format_version must be a canonical semantic version"))
	}
	if p.HostKernelRelease == "" || strings.TrimSpace(p.HostKernelRelease) != p.HostKernelRelease || len(p.HostKernelRelease) > 255 {
		problems = append(problems, errors.New("runtime.host_kernel_release must be a non-empty canonical release string"))
	}
	switch p.CPUTemplate.Kind {
	case CPUTemplateNone:
		if p.CPUTemplate.Digest != "" {
			problems = append(problems, errors.New("runtime.cpu_template.digest must be empty for kind none"))
		}
	case CPUTemplateCustom:
		if !sha256sum.ValidDigest(p.CPUTemplate.Digest) {
			problems = append(problems, errors.New("runtime.cpu_template.digest must be a canonical SHA-256 digest for kind custom"))
		}
	default:
		problems = append(problems, errors.New("runtime.cpu_template.kind must be none or custom"))
	}
	return errors.Join(problems...)
}

func ArchitectureFromGo(value string) (string, error) {
	if value == "amd64" {
		return "x86_64", nil
	}
	return "", fmt.Errorf("unsupported Go architecture %q", value)
}

func validSemanticVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return false
		}
	}
	return true
}
