package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	maxCPUFingerprintBytes     = int64(16 << 20)
	maxRuntimeProbeOutputBytes = 64 << 10
)

var (
	firecrackerVersionOutputPattern = regexp.MustCompile(`\AFirecracker v((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))\n\n\z`)
	snapshotVersionOutputPattern    = regexp.MustCompile(`\Av((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))\n\z`)
	firecrackerSuccessLogPattern    = regexp.MustCompile(`\A[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{9} \[anonymous-instance:main\] Firecracker exiting successfully\. exit_code=0\n\z`)
)

// HostRuntimeEvidence is the focused, measured input for the full runtime
// profile and a pool's ordered CPU-shape map. RuntimeCapabilities remains
// content-only artifact metadata.
type HostRuntimeEvidence struct {
	RuntimeID                 string                     `json:"runtime_id"`
	RuntimeArch               string                     `json:"runtime_arch"`
	VMRuntimeContract         string                     `json:"vm_runtime_contract"`
	FirecrackerDigest         string                     `json:"firecracker_digest"`
	FirecrackerVersion        string                     `json:"firecracker_version"`
	SnapshotFormatVersion     string                     `json:"snapshot_format_version"`
	CPUTemplateHelperDigest   string                     `json:"cpu_template_helper_digest"`
	HostKernelRelease         string                     `json:"host_kernel_release"`
	VMRuntimeDescriptorDigest string                     `json:"vm_runtime_descriptor_digest"`
	CPUTemplateSelector       CPUTemplateSelector        `json:"cpu_template_selector"`
	CPUShapes                 []CPUShapeEvidence         `json:"cpu_shapes"`
	Environment               RuntimeEnvironmentEvidence `json:"environment"`
	EnvironmentDigest         string                     `json:"environment_digest"`
	KernelDigest              string                     `json:"kernel_digest"`
	InitramfsDigest           string                     `json:"initramfs_digest"`
	RootfsDigest              string                     `json:"rootfs_digest"`

	firecrackerPath string
}

type CPUShapeEvidence struct {
	VCPUCount       int64  `json:"vcpu_count"`
	CPUConfigDigest string `json:"cpu_config_digest"`
}

type boundHostRuntimeEvidence struct {
	identity        runtimeid.Selector
	shapes          []CPUShapeEvidence
	firecrackerPath string
}

type hostRuntimeEvidenceStore struct {
	value atomic.Pointer[boundHostRuntimeEvidence]
}

func newHostRuntimeEvidenceStore() *hostRuntimeEvidenceStore {
	return &hostRuntimeEvidenceStore{}
}

func (store *hostRuntimeEvidenceStore) bind(evidence HostRuntimeEvidence, maxVCPUCount int64) error {
	if store == nil {
		return errors.New("host runtime evidence store is not configured")
	}
	if err := ValidateCPUShapeEvidence(evidence.CPUShapes, maxVCPUCount); err != nil {
		return err
	}
	identity, err := evidence.RuntimeIdentity()
	if err != nil {
		return err
	}
	if evidence.firecrackerPath == "" || !filepath.IsAbs(evidence.firecrackerPath) {
		return errors.New("host runtime evidence pinned Firecracker path is not canonical")
	}
	if err := validatePinnedRuntimeExecutable(evidence.firecrackerPath, identity.FirecrackerDigest); err != nil {
		return err
	}
	candidate := &boundHostRuntimeEvidence{
		identity:        identity,
		shapes:          append([]CPUShapeEvidence(nil), evidence.CPUShapes...),
		firecrackerPath: evidence.firecrackerPath,
	}
	if store.value.CompareAndSwap(nil, candidate) {
		return nil
	}
	bound := store.value.Load()
	if bound.identity != candidate.identity || bound.firecrackerPath != candidate.firecrackerPath || !reflect.DeepEqual(bound.shapes, candidate.shapes) {
		return errors.New("host runtime evidence changed after it was bound to the Firecracker connector")
	}
	return nil
}

func (store *hostRuntimeEvidenceStore) runtimeIdentity() (runtimeid.Selector, error) {
	if store == nil {
		return runtimeid.Selector{}, errors.New("host runtime evidence is not bound to the Firecracker connector")
	}
	evidence := store.value.Load()
	if evidence == nil {
		return runtimeid.Selector{}, errors.New("host runtime evidence is not bound to the Firecracker connector")
	}
	return evidence.identity, nil
}

func (store *hostRuntimeEvidenceStore) cpuConfigDigest(vcpuCount int64) (string, error) {
	if store == nil {
		return "", errors.New("host runtime evidence is not bound to the Firecracker connector")
	}
	evidence := store.value.Load()
	if evidence == nil {
		return "", errors.New("host runtime evidence is not bound to the Firecracker connector")
	}
	if vcpuCount < 1 || vcpuCount > int64(len(evidence.shapes)) {
		return "", fmt.Errorf("CPU shape evidence has no entry for %d vCPUs", vcpuCount)
	}
	shape := evidence.shapes[vcpuCount-1]
	if shape.VCPUCount != vcpuCount || !sha256sum.ValidDigest(shape.CPUConfigDigest) {
		return "", fmt.Errorf("CPU shape evidence entry for %d vCPUs is invalid", vcpuCount)
	}
	return shape.CPUConfigDigest, nil
}

func (store *hostRuntimeEvidenceStore) firecrackerExecutable() (string, error) {
	if store == nil {
		return "", errors.New("host runtime evidence is not bound to the Firecracker connector")
	}
	evidence := store.value.Load()
	if evidence == nil {
		return "", errors.New("host runtime evidence is not bound to the Firecracker connector")
	}
	if err := validatePinnedRuntimeExecutable(evidence.firecrackerPath, evidence.identity.FirecrackerDigest); err != nil {
		return "", err
	}
	return evidence.firecrackerPath, nil
}

// RuntimeIdentity returns the complete canonical runtime selector bound by
// this evidence. The ID is recomputed and checked rather than trusted.
func (evidence HostRuntimeEvidence) RuntimeIdentity() (runtimeid.Selector, error) {
	identity, err := deriveHostRuntimeIdentity(evidence)
	if err != nil {
		return runtimeid.Selector{}, err
	}
	if evidence.RuntimeID == "" {
		return runtimeid.Selector{}, errors.New("host runtime evidence runtime ID is required")
	}
	if evidence.RuntimeID != identity.ID {
		return runtimeid.Selector{}, fmt.Errorf(
			"host runtime evidence runtime ID %s does not match canonical ID %s",
			evidence.RuntimeID,
			identity.ID,
		)
	}
	return identity, nil
}

func deriveHostRuntimeIdentity(evidence HostRuntimeEvidence) (runtimeid.Selector, error) {
	if err := evidence.CPUTemplateSelector.Validate(); err != nil {
		return runtimeid.Selector{}, err
	}
	var cpuTemplate capacityapi.CPUTemplateSelector
	switch evidence.CPUTemplateSelector.Kind {
	case CPUTemplateNone:
		cpuTemplate.Kind = capacityapi.CPUTemplateNone
	case CPUTemplateCustom:
		cpuTemplate.Kind = capacityapi.CPUTemplateCustom
		cpuTemplate.Digest = evidence.CPUTemplateSelector.Digest
	default:
		return runtimeid.Selector{}, fmt.Errorf("CPU template selector kind %q is not supported", evidence.CPUTemplateSelector.Kind)
	}
	identity := runtimeid.Selector{
		Arch:                      evidence.RuntimeArch,
		Contract:                  evidence.VMRuntimeContract,
		VMRuntimeDescriptorDigest: evidence.VMRuntimeDescriptorDigest,
		FirecrackerDigest:         evidence.FirecrackerDigest,
		FirecrackerVersion:        evidence.FirecrackerVersion,
		SnapshotFormatVersion:     evidence.SnapshotFormatVersion,
		HostKernelRelease:         evidence.HostKernelRelease,
		CPUTemplate:               cpuTemplate,
		KernelDigest:              evidence.KernelDigest,
		InitramfsDigest:           evidence.InitramfsDigest,
		RootfsDigest:              evidence.RootfsDigest,
	}
	var err error
	identity.ID, err = runtimeid.Digest(identity)
	if err != nil {
		return runtimeid.Selector{}, fmt.Errorf("derive host runtime identity: %w", err)
	}
	return identity, nil
}

// RuntimeEnvironmentEvidence retains the helper fingerprint fields that are
// diagnostic rather than restore equality fences. guest_cpu_config is kept in
// the ordered CPUShapes map through its canonical digest.
type RuntimeEnvironmentEvidence struct {
	FirecrackerVersion string `json:"firecracker_version"`
	KernelVersion      string `json:"kernel_version"`
	MicrocodeVersion   string `json:"microcode_version"`
	BIOSVersion        string `json:"bios_version"`
	BIOSRevision       string `json:"bios_revision"`
}

func ValidateCPUShapeEvidence(shapes []CPUShapeEvidence, maxVCPUCount int64) error {
	if maxVCPUCount < 1 || maxVCPUCount > MaxVMVCPUCount {
		return fmt.Errorf("CPU shape maximum %d is outside [1,%d]", maxVCPUCount, MaxVMVCPUCount)
	}
	if int64(len(shapes)) != maxVCPUCount {
		return fmt.Errorf("CPU shape evidence has %d entries, want %d", len(shapes), maxVCPUCount)
	}
	for index, shape := range shapes {
		expected := int64(index + 1)
		if shape.VCPUCount != expected {
			return fmt.Errorf("CPU shape evidence entry %d has vCPU count %d, want %d", index, shape.VCPUCount, expected)
		}
		if !sha256sum.ValidDigest(shape.CPUConfigDigest) {
			return fmt.Errorf("CPU shape evidence for %d vCPUs has a noncanonical configuration digest", shape.VCPUCount)
		}
	}
	return nil
}

type runtimeProbeDependencies struct {
	run          func(context.Context, string, ...string) ([]byte, error)
	lookPath     func(string) (string, error)
	unameRelease func() (string, error)
}

func runRuntimeProbeCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, path, args...)
	stdout := &boundedCommandOutput{maximum: maxRuntimeProbeOutputBytes}
	stderr := &boundedCommandOutput{maximum: maxRuntimeProbeOutputBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if stdout.exceeded || stderr.exceeded || stdout.Len()+stderr.Len() > maxRuntimeProbeOutputBytes {
		return nil, fmt.Errorf("run %s: output exceeds %d bytes", filepath.Base(path), maxRuntimeProbeOutputBytes)
	}
	if err != nil {
		details := strings.TrimSpace(stderr.String())
		if details == "" {
			details = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), fmt.Errorf("run %s: %w: %s", filepath.Base(path), err, details)
	}
	return stdout.Bytes(), nil
}

type boundedCommandOutput struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	maximum  int
	exceeded bool
}

func (output *boundedCommandOutput) Write(contents []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	written := len(contents)
	remaining := output.maximum - output.buffer.Len()
	if remaining < len(contents) {
		output.exceeded = true
	}
	if remaining > 0 {
		if remaining > len(contents) {
			remaining = len(contents)
		}
		_, _ = output.buffer.Write(contents[:remaining])
	}
	return written, nil
}

func (output *boundedCommandOutput) Bytes() []byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	return append([]byte(nil), output.buffer.Bytes()...)
}

func (output *boundedCommandOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.String()
}

func (output *boundedCommandOutput) Len() int {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.Len()
}

func inspectHostRuntime(
	ctx context.Context,
	cfg Config,
	artifacts runtimeArtifacts,
	dependencies runtimeProbeDependencies,
) (HostRuntimeEvidence, error) {
	if err := ctx.Err(); err != nil {
		return HostRuntimeEvidence{}, err
	}
	if dependencies.run == nil || dependencies.lookPath == nil || dependencies.unameRelease == nil {
		return HostRuntimeEvidence{}, errors.New("host runtime inspection dependencies are incomplete")
	}
	cfg = cfg.WithDefaults()
	if err := validateRuntimeProbeConfig(cfg); err != nil {
		return HostRuntimeEvidence{}, err
	}

	firecrackerPath, err := resolveRuntimeProbeCommand(cfg.FirecrackerPath, dependencies.lookPath)
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("resolve Firecracker executable: %w", err)
	}
	helperPath, err := resolveRuntimeProbeCommand(cfg.CPUTemplateHelperPath, dependencies.lookPath)
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("resolve CPU template helper executable: %w", err)
	}
	firecrackerDigest, err := digestFile(firecrackerPath)
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("hash Firecracker executable %q: %w", firecrackerPath, err)
	}
	helperDigest, err := digestFile(helperPath)
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("hash CPU template helper executable %q: %w", helperPath, err)
	}
	versionOutput, err := dependencies.run(ctx, firecrackerPath, "--version")
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("inspect Firecracker version: %w", err)
	}
	firecrackerVersion, err := parseFirecrackerVersion(versionOutput)
	if err != nil {
		return HostRuntimeEvidence{}, err
	}
	snapshotOutput, err := dependencies.run(ctx, firecrackerPath, "--snapshot-version")
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("inspect Firecracker snapshot format version: %w", err)
	}
	snapshotVersion, err := parseSnapshotFormatVersion(snapshotOutput)
	if err != nil {
		return HostRuntimeEvidence{}, err
	}
	hostKernelRelease, err := dependencies.unameRelease()
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("inspect host kernel release: %w", err)
	}
	if hostKernelRelease == "" || hostKernelRelease != strings.TrimSpace(hostKernelRelease) {
		return HostRuntimeEvidence{}, errors.New("host kernel release is not canonical")
	}
	descriptorDigest, err := CanonicalVMRuntimeDescriptor().Digest()
	if err != nil {
		return HostRuntimeEvidence{}, err
	}
	runtimeArchitecture, err := runtimeid.ArchitectureFromGo(artifacts.Arch)
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("derive runtime architecture from artifacts: %w", err)
	}

	selector := cfg.CPUTemplateSelector.withDefaults()
	templatePath := ""
	templateDigest := ""
	if selector.Kind == CPUTemplateCustom {
		templatePath, err = resolveRuntimeProbeFile(cfg.CustomCPUTemplatePath)
		if err != nil {
			return HostRuntimeEvidence{}, fmt.Errorf("resolve custom CPU template: %w", err)
		}
		templateDigest, err = digestFile(templatePath)
		if err != nil {
			return HostRuntimeEvidence{}, fmt.Errorf("hash custom CPU template %q: %w", templatePath, err)
		}
		if templateDigest != selector.Digest {
			return HostRuntimeEvidence{}, fmt.Errorf("custom CPU template digest %s does not match selector %s", templateDigest, selector.Digest)
		}
	}

	directory, err := os.MkdirTemp("", "helmr-runtime-probe-")
	if err != nil {
		return HostRuntimeEvidence{}, fmt.Errorf("create host runtime inspection directory: %w", err)
	}
	defer os.RemoveAll(directory)

	shapes := make([]CPUShapeEvidence, 0, cfg.VCPUCount)
	var environment RuntimeEnvironmentEvidence
	for vcpuCount := int64(1); vcpuCount <= cfg.VCPUCount; vcpuCount++ {
		if err := ctx.Err(); err != nil {
			return HostRuntimeEvidence{}, err
		}
		configBytes, err := canonicalCPUTemplateHelperConfig(cfg, vcpuCount)
		if err != nil {
			return HostRuntimeEvidence{}, err
		}
		configPath := filepath.Join(directory, fmt.Sprintf("config-%02d.json", vcpuCount))
		if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
			return HostRuntimeEvidence{}, fmt.Errorf("write CPU template helper config for %d vCPUs: %w", vcpuCount, err)
		}
		if selector.Kind == CPUTemplateCustom {
			output, err := dependencies.run(
				ctx, helperPath, "template", "verify", "--template", templatePath, "--config", configPath,
			)
			if err != nil {
				return HostRuntimeEvidence{}, fmt.Errorf("verify custom CPU template for %d vCPUs: %w", vcpuCount, err)
			}
			if len(output) != 0 {
				return HostRuntimeEvidence{}, fmt.Errorf("verify custom CPU template for %d vCPUs produced unexpected output", vcpuCount)
			}
		}

		fingerprintPath := filepath.Join(directory, fmt.Sprintf("fingerprint-%02d.json", vcpuCount))
		arguments := []string{"fingerprint", "dump", "--output", fingerprintPath, "--config", configPath}
		if selector.Kind == CPUTemplateCustom {
			arguments = append(arguments, "--template", templatePath)
		}
		output, err := dependencies.run(ctx, helperPath, arguments...)
		if err != nil {
			return HostRuntimeEvidence{}, fmt.Errorf("dump CPU fingerprint for %d vCPUs: %w", vcpuCount, err)
		}
		if len(output) != 0 {
			return HostRuntimeEvidence{}, fmt.Errorf("dump CPU fingerprint for %d vCPUs produced unexpected output", vcpuCount)
		}
		fingerprintBytes, err := readBoundedFile(fingerprintPath, maxCPUFingerprintBytes)
		if err != nil {
			return HostRuntimeEvidence{}, fmt.Errorf("read CPU fingerprint for %d vCPUs: %w", vcpuCount, err)
		}
		fingerprint, err := decodeCPUFingerprint(fingerprintBytes)
		if err != nil {
			return HostRuntimeEvidence{}, fmt.Errorf("decode CPU fingerprint for %d vCPUs: %w", vcpuCount, err)
		}
		if fingerprint.Environment.FirecrackerVersion != firecrackerVersion {
			return HostRuntimeEvidence{}, fmt.Errorf(
				"CPU template helper Firecracker version %q does not match executed Firecracker version %q",
				fingerprint.Environment.FirecrackerVersion, firecrackerVersion,
			)
		}
		if fingerprint.Environment.KernelVersion != hostKernelRelease {
			return HostRuntimeEvidence{}, fmt.Errorf(
				"CPU template helper kernel version %q does not match uname release %q",
				fingerprint.Environment.KernelVersion, hostKernelRelease,
			)
		}
		if vcpuCount == 1 {
			environment = fingerprint.Environment
		} else if !reflect.DeepEqual(environment, fingerprint.Environment) {
			return HostRuntimeEvidence{}, fmt.Errorf("CPU template helper environment changed while probing %d vCPUs", vcpuCount)
		}
		shapes = append(shapes, CPUShapeEvidence{
			VCPUCount: vcpuCount, CPUConfigDigest: sha256sum.DigestBytes(fingerprint.GuestCPUConfig),
		})
	}
	if err := ValidateCPUShapeEvidence(shapes, cfg.VCPUCount); err != nil {
		return HostRuntimeEvidence{}, err
	}
	if err := requireStableFileDigest("Firecracker executable", firecrackerPath, firecrackerDigest); err != nil {
		return HostRuntimeEvidence{}, err
	}
	if err := requireStableFileDigest("CPU template helper executable", helperPath, helperDigest); err != nil {
		return HostRuntimeEvidence{}, err
	}
	if selector.Kind == CPUTemplateCustom {
		if err := requireStableFileDigest("custom CPU template", templatePath, templateDigest); err != nil {
			return HostRuntimeEvidence{}, err
		}
	}
	environmentDigest, err := digestRuntimeEnvironment(environment)
	if err != nil {
		return HostRuntimeEvidence{}, err
	}
	evidence := HostRuntimeEvidence{
		RuntimeArch: runtimeArchitecture, VMRuntimeContract: artifacts.VMRuntimeContract,
		FirecrackerDigest: firecrackerDigest, FirecrackerVersion: firecrackerVersion,
		SnapshotFormatVersion: snapshotVersion, CPUTemplateHelperDigest: helperDigest,
		HostKernelRelease:         hostKernelRelease,
		VMRuntimeDescriptorDigest: descriptorDigest, CPUTemplateSelector: selector,
		CPUShapes: shapes, Environment: environment, EnvironmentDigest: environmentDigest,
		KernelDigest: artifacts.Kernel.Digest, InitramfsDigest: artifacts.Initramfs.Digest,
		RootfsDigest:    artifacts.Rootfs.Digest,
		firecrackerPath: firecrackerPath,
	}
	identity, err := deriveHostRuntimeIdentity(evidence)
	if err != nil {
		return HostRuntimeEvidence{}, err
	}
	evidence.RuntimeID = identity.ID
	return evidence, nil
}

func validateRuntimeProbeConfig(cfg Config) error {
	var problems []error
	for name, value := range map[string]string{
		"Firecracker": cfg.FirecrackerPath, "CPU template helper": cfg.CPUTemplateHelperPath,
		"guest kernel": cfg.KernelPath, "guest initramfs": cfg.InitramfsPath, "guest rootfs": cfg.RootfsPath,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			problems = append(problems, fmt.Errorf("%s path is not canonical", name))
		}
	}
	if cfg.VCPUCount < 1 || cfg.VCPUCount > MaxVMVCPUCount {
		problems = append(problems, fmt.Errorf("guest vCPU count %d is outside [1,%d]", cfg.VCPUCount, MaxVMVCPUCount))
	}
	if cfg.MemoryMiB <= 0 {
		problems = append(problems, fmt.Errorf("guest memory must be positive, got %d MiB", cfg.MemoryMiB))
	}
	if err := cfg.CPUTemplateSelector.Validate(); err != nil {
		problems = append(problems, err)
	}
	switch cfg.CPUTemplateSelector.Kind {
	case CPUTemplateNone:
		if cfg.CustomCPUTemplatePath != "" {
			problems = append(problems, errors.New("no-template CPU selector must not include a template path"))
		}
	case CPUTemplateCustom:
		if cfg.CustomCPUTemplatePath == "" || cfg.CustomCPUTemplatePath != strings.TrimSpace(cfg.CustomCPUTemplatePath) {
			problems = append(problems, errors.New("custom CPU template path is not canonical"))
		}
	}
	return errors.Join(problems...)
}

func resolveRuntimeProbeCommand(path string, lookPath func(string) (string, error)) (string, error) {
	resolved := path
	if filepath.Base(path) == path {
		var err error
		resolved, err = lookPath(path)
		if err != nil {
			return "", err
		}
	}
	if resolved == "" {
		return "", errors.New("resolved command path is empty")
	}
	return resolveRuntimeProbeFile(resolved)
}

func resolveRuntimeProbeFile(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return canonical, nil
}

func parseFirecrackerVersion(output []byte) (string, error) {
	primary, err := firecrackerProbePrimaryOutput(output)
	if err != nil {
		return "", fmt.Errorf("firecracker --version output %q is not canonical", output)
	}
	matches := firecrackerVersionOutputPattern.FindSubmatch(primary)
	if len(matches) != 2 {
		return "", fmt.Errorf("firecracker --version output %q is not canonical", output)
	}
	return string(matches[1]), nil
}

func parseSnapshotFormatVersion(output []byte) (string, error) {
	primary, err := firecrackerProbePrimaryOutput(output)
	if err != nil {
		return "", fmt.Errorf("firecracker --snapshot-version output %q is not canonical", output)
	}
	matches := snapshotVersionOutputPattern.FindSubmatch(primary)
	if len(matches) != 2 {
		return "", fmt.Errorf("firecracker --snapshot-version output %q is not canonical", output)
	}
	return string(matches[1]), nil
}

func firecrackerProbePrimaryOutput(output []byte) ([]byte, error) {
	lastNewline := bytes.LastIndexByte(output, '\n')
	if lastNewline != len(output)-1 {
		return nil, errors.New("Firecracker probe output is not newline terminated")
	}
	previousNewline := bytes.LastIndexByte(output[:lastNewline], '\n')
	if previousNewline < 0 {
		return output, nil
	}
	diagnostic := output[previousNewline+1:]
	if !firecrackerSuccessLogPattern.Match(diagnostic) {
		return output, nil
	}
	return output[:previousNewline+1], nil
}

type decodedCPUFingerprint struct {
	Environment    RuntimeEnvironmentEvidence
	GuestCPUConfig []byte
}

func decodeCPUFingerprint(raw []byte) (decodedCPUFingerprint, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return decodedCPUFingerprint{}, err
	}
	var document struct {
		FirecrackerVersion *string          `json:"firecracker_version"`
		KernelVersion      *string          `json:"kernel_version"`
		MicrocodeVersion   *string          `json:"microcode_version"`
		BIOSVersion        *string          `json:"bios_version"`
		BIOSRevision       *string          `json:"bios_revision"`
		GuestCPUConfig     *json.RawMessage `json:"guest_cpu_config"`
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return decodedCPUFingerprint{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return decodedCPUFingerprint{}, errors.New("CPU fingerprint has trailing data")
		}
		return decodedCPUFingerprint{}, fmt.Errorf("decode CPU fingerprint trailing data: %w", err)
	}
	fields := []struct {
		name  string
		value *string
	}{
		{name: "firecracker_version", value: document.FirecrackerVersion},
		{name: "kernel_version", value: document.KernelVersion},
		{name: "microcode_version", value: document.MicrocodeVersion},
		{name: "bios_version", value: document.BIOSVersion},
		{name: "bios_revision", value: document.BIOSRevision},
	}
	for _, field := range fields {
		if field.value == nil {
			return decodedCPUFingerprint{}, fmt.Errorf("CPU fingerprint field %s is missing", field.name)
		}
		if *field.value == "" {
			return decodedCPUFingerprint{}, fmt.Errorf("CPU fingerprint field %s is empty", field.name)
		}
	}
	if document.GuestCPUConfig == nil {
		return decodedCPUFingerprint{}, errors.New("CPU fingerprint field guest_cpu_config is missing")
	}
	guestCPUConfig, err := jsoncanon.Transform(*document.GuestCPUConfig)
	if err != nil {
		return decodedCPUFingerprint{}, fmt.Errorf("canonicalize guest_cpu_config: %w", err)
	}
	if len(guestCPUConfig) == 0 || guestCPUConfig[0] != '{' {
		return decodedCPUFingerprint{}, errors.New("CPU fingerprint guest_cpu_config must be a JSON object")
	}
	return decodedCPUFingerprint{
		Environment: RuntimeEnvironmentEvidence{
			FirecrackerVersion: *document.FirecrackerVersion,
			KernelVersion:      *document.KernelVersion,
			MicrocodeVersion:   *document.MicrocodeVersion,
			BIOSVersion:        *document.BIOSVersion,
			BIOSRevision:       *document.BIOSRevision,
		},
		GuestCPUConfig: guestCPUConfig,
	}, nil
}

func digestRuntimeEnvironment(environment RuntimeEnvironmentEvidence) (string, error) {
	raw, err := json.Marshal(environment)
	if err != nil {
		return "", fmt.Errorf("encode runtime environment evidence: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize runtime environment evidence: %w", err)
	}
	return sha256sum.DigestBytes(canonical), nil
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return "", statErr
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return "", errors.New("file is not regular")
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	return sha256sum.DigestHash(hash), nil
}

func requireStableFileDigest(name string, path string, expected string) error {
	actual, err := digestFile(path)
	if err != nil {
		return fmt.Errorf("rehash %s %q: %w", name, path, err)
	}
	if actual != expected {
		return fmt.Errorf("%s %q changed during host runtime inspection", name, path)
	}
	return nil
}

// pinRuntimeExecutable copies measured Firecracker bytes into a private,
// content-addressed path. Sessions execute only this path, so replacing the
// configured command or retargeting a symlink after inspection cannot change
// the binary that was bound into the host runtime identity.
func pinRuntimeExecutable(sourcePath string, stateDir string, expectedDigest string) (string, error) {
	if !filepath.IsAbs(sourcePath) {
		return "", errors.New("measured Firecracker executable path is not absolute")
	}
	if !sha256sum.ValidDigest(expectedDigest) {
		return "", errors.New("measured Firecracker executable digest is not canonical")
	}
	if strings.TrimSpace(stateDir) == "" {
		return "", errors.New("firecracker state directory is required to pin the measured executable")
	}
	absoluteStateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve Firecracker state directory: %w", err)
	}
	digestDirectory := filepath.Join(
		filepath.Dir(filepath.Clean(absoluteStateDir)),
		"runtime-bin",
		strings.TrimPrefix(expectedDigest, "sha256:"),
	)
	if err := os.MkdirAll(digestDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create pinned Firecracker directory: %w", err)
	}
	if err := os.Chmod(digestDirectory, 0o700); err != nil {
		return "", fmt.Errorf("secure pinned Firecracker directory: %w", err)
	}
	targetPath := filepath.Join(digestDirectory, "firecracker")
	if err := validatePinnedRuntimeExecutable(targetPath, expectedDigest); err == nil {
		return targetPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open measured Firecracker executable: %w", err)
	}
	sourceInfo, statErr := source.Stat()
	if statErr != nil || !sourceInfo.Mode().IsRegular() {
		_ = source.Close()
		if statErr != nil {
			return "", fmt.Errorf("inspect measured Firecracker executable: %w", statErr)
		}
		return "", errors.New("measured Firecracker executable is not a regular file")
	}

	temporary, err := os.CreateTemp(digestDirectory, ".firecracker-")
	if err != nil {
		_ = source.Close()
		return "", fmt.Errorf("create pinned Firecracker executable: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(temporary, hash), source)
	syncErr := temporary.Sync()
	chmodErr := temporary.Chmod(0o500)
	closeErr := errors.Join(source.Close(), temporary.Close())
	if err := errors.Join(copyErr, syncErr, chmodErr, closeErr); err != nil {
		return "", fmt.Errorf("pin measured Firecracker executable: %w", err)
	}
	if actualDigest := sha256sum.DigestHash(hash); actualDigest != expectedDigest {
		return "", fmt.Errorf("measured Firecracker executable %q changed while it was pinned", sourcePath)
	}
	if err := os.Link(temporaryPath, targetPath); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("publish pinned Firecracker executable: %w", err)
	}
	if err := validatePinnedRuntimeExecutable(targetPath, expectedDigest); err != nil {
		return "", err
	}
	return targetPath, nil
}

func validatePinnedRuntimeExecutable(path string, expectedDigest string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("pinned Firecracker executable %q is not a regular file", path)
	}
	if info.Mode().Perm() != 0o500 {
		return fmt.Errorf("pinned Firecracker executable %q has unsafe permissions %04o", path, info.Mode().Perm())
	}
	if err := requireStableFileDigest("pinned Firecracker executable", path, expectedDigest); err != nil {
		return err
	}
	return nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	reader := io.LimitReader(file, maximum+1)
	contents, readErr := io.ReadAll(reader)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if int64(len(contents)) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return contents, nil
}
