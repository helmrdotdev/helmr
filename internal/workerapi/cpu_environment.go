package workerapi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const cpuEnvironmentDomain = "helmr.cpu-environment.v0"

func (e CPUEnvironment) ExpectedDigest() (string, error) {
	if err := e.validateFields(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Domain             string `json:"domain"`
		FirecrackerVersion string `json:"firecracker_version"`
		HostKernelRelease  string `json:"host_kernel_release"`
		MicrocodeVersion   string `json:"microcode_version"`
		BIOSVersion        string `json:"bios_version"`
		BIOSRevision       string `json:"bios_revision"`
	}{
		Domain: cpuEnvironmentDomain, FirecrackerVersion: e.FirecrackerVersion,
		HostKernelRelease: e.HostKernelRelease, MicrocodeVersion: e.MicrocodeVersion,
		BIOSVersion: e.BIOSVersion, BIOSRevision: e.BIOSRevision,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return sha256sum.FormatDigest(sum[:]), nil
}

func (e CPUEnvironment) Validate() error {
	expected, err := e.ExpectedDigest()
	if err != nil {
		return err
	}
	if e.Digest != expected {
		return errors.New("cpu environment digest does not match its canonical fields")
	}
	return nil
}

func (e CPUEnvironment) validateFields() error {
	for _, value := range []string{
		e.FirecrackerVersion,
		e.HostKernelRelease,
		e.MicrocodeVersion,
		e.BIOSVersion,
		e.BIOSRevision,
	} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 255 {
			return errors.New("cpu environment fields must be non-empty canonical strings")
		}
	}
	return nil
}
