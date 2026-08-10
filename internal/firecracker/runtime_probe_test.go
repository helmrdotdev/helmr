package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestInspectHostRuntimeReturnsOrderedCanonicalEvidence(t *testing.T) {
	directory := t.TempDir()
	firecrackerPath := writeProbeTestFile(t, directory, "firecracker", "firecracker-binary")
	helperPath := writeProbeTestFile(t, directory, "cpu-template-helper", "helper-binary")
	kernelPath := writeProbeTestFile(t, directory, "vmlinux", "kernel")
	initramfsPath := writeProbeTestFile(t, directory, "initramfs", "initramfs")
	rootfsPath := writeProbeTestFile(t, directory, "rootfs.squashfs", "rootfs")

	var probedVCPUs []int64
	dependencies := runtimeProbeDependencies{
		lookPath:     func(path string) (string, error) { return path, nil },
		unameRelease: func() (string, error) { return "6.12.31-amzn2023", nil },
		run: func(_ context.Context, path string, arguments ...string) ([]byte, error) {
			switch path {
			case firecrackerPath:
				switch {
				case slices.Equal(arguments, []string{"--version"}):
					return []byte("Firecracker v1.16.1\n\n"), nil
				case slices.Equal(arguments, []string{"--snapshot-version"}):
					return []byte("v5.0.0\n"), nil
				default:
					return nil, fmt.Errorf("unexpected Firecracker arguments %v", arguments)
				}
			case helperPath:
				if len(arguments) != 6 || !slices.Equal(arguments[:2], []string{"fingerprint", "dump"}) {
					return nil, fmt.Errorf("unexpected helper arguments %v", arguments)
				}
				outputPath := flagValue(t, arguments, "--output")
				configPath := flagValue(t, arguments, "--config")
				var config cpuTemplateHelperConfig
				configBytes, err := os.ReadFile(configPath)
				if err != nil {
					return nil, err
				}
				if err := json.Unmarshal(configBytes, &config); err != nil {
					return nil, err
				}
				if config.MachineConfig.SMT {
					return nil, errors.New("helper config enabled SMT")
				}
				probedVCPUs = append(probedVCPUs, config.MachineConfig.VCPUCount)
				guestConfig := fmt.Sprintf(`{"cpuid_modifiers":[{"leaf":"0x%x"}],"msr_modifiers":[]}`, config.MachineConfig.VCPUCount)
				fingerprint := fmt.Sprintf(`{
  "guest_cpu_config": %s,
  "bios_revision": "3.5",
  "microcode_version": "0x2b000643",
  "firecracker_version": "1.16.1",
  "bios_version": "1.0",
  "kernel_version": "6.12.31-amzn2023"
}`, guestConfig)
				return nil, os.WriteFile(outputPath, []byte(fingerprint), 0o600)
			default:
				return nil, fmt.Errorf("unexpected command %q", path)
			}
		},
	}
	artifacts := runtimeArtifacts{
		Arch:              "amd64",
		VMRuntimeContract: runtimeid.Contract,
		Kernel:            runtimeArtifact{Digest: testCanonicalDigest("1")},
		Initramfs:         runtimeArtifact{Digest: testCanonicalDigest("2")},
		Rootfs:            runtimeArtifact{Digest: testCanonicalDigest("3")},
	}
	evidence, err := inspectHostRuntime(context.Background(), Config{
		FirecrackerPath: firecrackerPath, CPUTemplateHelperPath: helperPath,
		KernelPath: kernelPath, InitramfsPath: initramfsPath, RootfsPath: rootfsPath,
		VCPUCount: 3, MemoryMiB: 2048,
	}, artifacts, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(probedVCPUs, []int64{1, 2, 3}) {
		t.Fatalf("probed vCPUs = %v", probedVCPUs)
	}
	if err := ValidateCPUShapeEvidence(evidence.CPUShapes, 3); err != nil {
		t.Fatal(err)
	}
	for index, shape := range evidence.CPUShapes {
		guestConfig := fmt.Sprintf(`{"msr_modifiers":[],"cpuid_modifiers":[{"leaf":"0x%x"}]}`, index+1)
		canonical, err := jsoncanon.Transform([]byte(guestConfig))
		if err != nil {
			t.Fatal(err)
		}
		if shape.CPUConfigDigest != sha256sum.DigestBytes(canonical) {
			t.Fatalf("shape %d digest = %s", index+1, shape.CPUConfigDigest)
		}
	}
	if evidence.FirecrackerDigest != sha256sum.DigestBytes([]byte("firecracker-binary")) ||
		evidence.CPUTemplateHelperDigest != sha256sum.DigestBytes([]byte("helper-binary")) ||
		evidence.FirecrackerVersion != "1.16.1" || evidence.SnapshotFormatVersion != "5.0.0" ||
		evidence.HostKernelRelease != "6.12.31-amzn2023" || evidence.CPUTemplateSelector != NoCPUTemplateSelector() {
		t.Fatalf("evidence = %+v", evidence)
	}
	descriptorDigest, err := CanonicalVMRuntimeDescriptor().Digest()
	if err != nil {
		t.Fatal(err)
	}
	if evidence.VMRuntimeDescriptorDigest != descriptorDigest || evidence.KernelDigest != artifacts.Kernel.Digest || evidence.InitramfsDigest != artifacts.Initramfs.Digest || evidence.RootfsDigest != artifacts.Rootfs.Digest {
		t.Fatalf("evidence = %+v", evidence)
	}
	identity, err := evidence.RuntimeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != evidence.RuntimeID || identity.Arch != "x86_64" || identity.Contract != runtimeid.Contract || identity.FirecrackerDigest != evidence.FirecrackerDigest || identity.VMRuntimeDescriptorDigest != descriptorDigest {
		t.Fatalf("runtime identity = %+v evidence = %+v", identity, evidence)
	}
	expectedEnvironment := RuntimeEnvironmentEvidence{
		FirecrackerVersion: "1.16.1", KernelVersion: "6.12.31-amzn2023",
		MicrocodeVersion: "0x2b000643", BIOSVersion: "1.0", BIOSRevision: "3.5",
	}
	expectedEnvironmentDigest, err := digestRuntimeEnvironment(expectedEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Environment != expectedEnvironment || evidence.EnvironmentDigest != expectedEnvironmentDigest {
		t.Fatalf("environment = %+v digest=%s", evidence.Environment, evidence.EnvironmentDigest)
	}
}

func TestInspectHostRuntimeVerifiesCustomTemplateForEveryShape(t *testing.T) {
	directory := t.TempDir()
	firecrackerPath := writeProbeTestFile(t, directory, "firecracker", "firecracker")
	helperPath := writeProbeTestFile(t, directory, "cpu-template-helper", "helper")
	templatePath := writeProbeTestFile(t, directory, "template.json", `{"cpuid_modifiers":[]}`)
	selector, err := CustomCPUTemplateSelector(sha256sum.DigestBytes([]byte(`{"cpuid_modifiers":[]}`)))
	if err != nil {
		t.Fatal(err)
	}
	var verified, dumped int
	dependencies := runtimeProbeDependencies{
		lookPath:     func(path string) (string, error) { return path, nil },
		unameRelease: func() (string, error) { return "6.12.31", nil },
		run: func(_ context.Context, path string, arguments ...string) ([]byte, error) {
			if path == firecrackerPath {
				if slices.Equal(arguments, []string{"--version"}) {
					return []byte("Firecracker v1.16.1\n\n"), nil
				}
				return []byte("v5.0.0\n"), nil
			}
			if path != helperPath {
				return nil, fmt.Errorf("unexpected command %q", path)
			}
			if slices.Equal(arguments[:2], []string{"template", "verify"}) {
				verified++
				if flagValue(t, arguments, "--template") != templatePath {
					return nil, errors.New("wrong template path")
				}
				return nil, nil
			}
			dumped++
			outputPath := flagValue(t, arguments, "--output")
			if flagValue(t, arguments, "--template") != templatePath {
				return nil, errors.New("wrong template path")
			}
			return nil, os.WriteFile(outputPath, []byte(`{
  "firecracker_version":"1.16.1","kernel_version":"6.12.31",
  "microcode_version":"0x1","bios_version":"1.0","bios_revision":"1.0",
  "guest_cpu_config":{"cpuid_modifiers":[]}
}`), 0o600)
		},
	}
	_, err = inspectHostRuntime(context.Background(), Config{
		FirecrackerPath: firecrackerPath, CPUTemplateHelperPath: helperPath,
		CPUTemplateSelector: selector, CustomCPUTemplatePath: templatePath,
		KernelPath:    writeProbeTestFile(t, directory, "kernel", "kernel"),
		InitramfsPath: writeProbeTestFile(t, directory, "initramfs", "initramfs"),
		RootfsPath:    writeProbeTestFile(t, directory, "rootfs", "rootfs"),
		VCPUCount:     2, MemoryMiB: 128,
	}, testProbeRuntimeArtifacts(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if verified != 2 || dumped != 2 {
		t.Fatalf("verified=%d dumped=%d", verified, dumped)
	}
}

func TestDecodeCPUFingerprintIsStrictAndCanonical(t *testing.T) {
	base := `{
  "firecracker_version":"1.16.1","kernel_version":"6.12.31",
  "microcode_version":"0x1","bios_version":"1.0","bios_revision":"1.0",
  "guest_cpu_config":{"b":2,"a":1}
}`
	fingerprint, err := decodeCPUFingerprint([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if string(fingerprint.GuestCPUConfig) != `{"a":1,"b":2}` {
		t.Fatalf("guest CPU config = %s", fingerprint.GuestCPUConfig)
	}
	for name, raw := range map[string]string{
		"unknown":          strings.Replace(base, `"guest_cpu_config"`, `"unknown":true,"guest_cpu_config"`, 1),
		"duplicate":        strings.Replace(base, `"kernel_version":"6.12.31"`, `"kernel_version":"6.12.31","kernel_version":"6.12.31"`, 1),
		"nested duplicate": strings.Replace(base, `{"b":2,"a":1}`, `{"b":2,"a":1,"a":1}`, 1),
		"missing":          strings.Replace(base, `"bios_revision":"1.0",`, "", 1),
		"null":             strings.Replace(base, `"bios_version":"1.0"`, `"bios_version":null`, 1),
		"wrong type":       strings.Replace(base, `"microcode_version":"0x1"`, `"microcode_version":1`, 1),
		"non-object guest": strings.Replace(base, `{"b":2,"a":1}`, `[]`, 1),
		"trailing":         base + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCPUFingerprint([]byte(raw)); err == nil {
				t.Fatal("decode succeeded")
			}
		})
	}
	invalidUTF8 := append([]byte(base[:len(base)-2]), 0xff, '}')
	if _, err := decodeCPUFingerprint(invalidUTF8); err == nil {
		t.Fatal("invalid UTF-8 fingerprint decoded")
	}
}

func TestRuntimeProbeVersionParsingIsStrict(t *testing.T) {
	if got, err := parseFirecrackerVersion([]byte("Firecracker v1.16.1\n\n")); err != nil || got != "1.16.1" {
		t.Fatalf("version=%q error=%v", got, err)
	}
	if got, err := parseFirecrackerVersion([]byte("Firecracker v1.16.1\n\n2026-08-10T22:41:48.709727839 [anonymous-instance:main] Firecracker exiting successfully. exit_code=0\n")); err != nil || got != "1.16.1" {
		t.Fatalf("version with exit diagnostic=%q error=%v", got, err)
	}
	if got, err := parseSnapshotFormatVersion([]byte("v5.0.0\n")); err != nil || got != "5.0.0" {
		t.Fatalf("snapshot version=%q error=%v", got, err)
	}
	if got, err := parseSnapshotFormatVersion([]byte("v10.0.0\n2026-08-10T22:46:48.310385047 [anonymous-instance:main] Firecracker exiting successfully. exit_code=0\n")); err != nil || got != "10.0.0" {
		t.Fatalf("snapshot version with exit diagnostic=%q error=%v", got, err)
	}
	for _, invalid := range [][]byte{
		[]byte("Firecracker v1.16.1\n"), []byte("Firecracker v01.16.1\n\n"),
		[]byte("prefix Firecracker v1.16.1\n\n"), []byte("Firecracker v1.16.1\n\nsuffix"),
		[]byte("Firecracker v1.16.1\n\n2026-08-10T22:41:48.709727839 [anonymous-instance:main] Firecracker exiting successfully. exit_code=1\n"),
		[]byte("Firecracker v1.16.1\n\n2026-08-10T22:41:48.709727839 [other:main] Firecracker exiting successfully. exit_code=0\n"),
	} {
		if _, err := parseFirecrackerVersion(invalid); err == nil {
			t.Fatalf("invalid Firecracker output %q was accepted", invalid)
		}
	}
	for _, invalid := range [][]byte{
		[]byte("v5.0.0\n\n"), []byte("v5.0\n"), []byte("v05.0.0\n"),
		[]byte("prefix v5.0.0\n"), []byte("v5.0.0\nsuffix"),
		[]byte("v10.0.0\n2026-08-10T22:46:48.310385047 [anonymous-instance:main] unexpected\n"),
	} {
		if _, err := parseSnapshotFormatVersion(invalid); err == nil {
			t.Fatalf("invalid snapshot output %q was accepted", invalid)
		}
	}
}

func TestRunRuntimeProbeCommandSeparatesDiagnosticStderr(t *testing.T) {
	output, err := runRuntimeProbeCommand(
		t.Context(),
		"/bin/sh",
		"-c",
		`printf 'Firecracker v1.16.1\n\n'; printf 'Firecracker exiting successfully. exit_code=0\n' >&2`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := parseFirecrackerVersion(output); err != nil || got != "1.16.1" {
		t.Fatalf("version=%q output=%q error=%v", got, output, err)
	}
}

func TestInspectHostRuntimeRejectsIdentityMutation(t *testing.T) {
	for _, target := range []string{"Firecracker", "CPU template helper", "custom CPU template"} {
		t.Run(target, func(t *testing.T) {
			directory := t.TempDir()
			firecrackerPath := writeProbeTestFile(t, directory, "firecracker", "firecracker")
			helperPath := writeProbeTestFile(t, directory, "cpu-template-helper", "helper")
			templateBody := `{"cpuid_modifiers":[]}`
			templatePath := writeProbeTestFile(t, directory, "template.json", templateBody)
			selector, err := CustomCPUTemplateSelector(sha256sum.DigestBytes([]byte(templateBody)))
			if err != nil {
				t.Fatal(err)
			}
			dependencies := runtimeProbeDependencies{
				lookPath:     func(path string) (string, error) { return path, nil },
				unameRelease: func() (string, error) { return "6.12.31", nil },
				run: func(_ context.Context, path string, arguments ...string) ([]byte, error) {
					if path == firecrackerPath {
						if slices.Equal(arguments, []string{"--version"}) {
							return []byte("Firecracker v1.16.1\n\n"), nil
						}
						if target == "Firecracker" {
							if err := os.WriteFile(firecrackerPath, []byte("changed"), 0o700); err != nil {
								return nil, err
							}
						}
						return []byte("v5.0.0\n"), nil
					}
					if path != helperPath {
						return nil, fmt.Errorf("unexpected command %q", path)
					}
					if slices.Equal(arguments[:2], []string{"template", "verify"}) {
						return nil, nil
					}
					if target == "CPU template helper" {
						if err := os.WriteFile(helperPath, []byte("changed"), 0o700); err != nil {
							return nil, err
						}
					}
					if target == "custom CPU template" {
						if err := os.WriteFile(templatePath, []byte("changed"), 0o600); err != nil {
							return nil, err
						}
					}
					fingerprint := `{
  "firecracker_version":"1.16.1","kernel_version":"6.12.31",
  "microcode_version":"0x1","bios_version":"1.0","bios_revision":"1.0",
  "guest_cpu_config":{"cpuid_modifiers":[]}
}`
					return nil, os.WriteFile(flagValue(t, arguments, "--output"), []byte(fingerprint), 0o600)
				},
			}
			_, err = inspectHostRuntime(context.Background(), Config{
				FirecrackerPath: firecrackerPath, CPUTemplateHelperPath: helperPath,
				CPUTemplateSelector: selector, CustomCPUTemplatePath: templatePath,
				KernelPath:    writeProbeTestFile(t, directory, "kernel", "kernel"),
				InitramfsPath: writeProbeTestFile(t, directory, "initramfs", "initramfs"),
				RootfsPath:    writeProbeTestFile(t, directory, "rootfs", "rootfs"),
				VCPUCount:     1, MemoryMiB: 128,
			}, testProbeRuntimeArtifacts(), dependencies)
			if err == nil || !strings.Contains(err.Error(), target) || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRunRuntimeProbeCommandBoundsCombinedOutput(t *testing.T) {
	t.Setenv("HELMR_RUNTIME_PROBE_OUTPUT_HELPER", "1")
	_, err := runRuntimeProbeCommand(
		context.Background(), os.Args[0], "-test.run=^TestRuntimeProbeCommandOutputHelper$",
	)
	if err == nil || !strings.Contains(err.Error(), "output exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestRuntimeProbeCommandOutputHelper(t *testing.T) {
	if os.Getenv("HELMR_RUNTIME_PROBE_OUTPUT_HELPER") != "1" {
		return
	}
	_, _ = os.Stdout.Write(make([]byte, maxRuntimeProbeOutputBytes+1))
	os.Exit(0)
}

func TestValidateCPUShapeEvidenceRequiresCompleteOrderedMap(t *testing.T) {
	digest := testCanonicalDigest("a")
	valid := []CPUShapeEvidence{{VCPUCount: 1, CPUConfigDigest: digest}, {VCPUCount: 2, CPUConfigDigest: digest}}
	if err := ValidateCPUShapeEvidence(valid, 2); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]CPUShapeEvidence{
		valid[:1],
		{{VCPUCount: 2, CPUConfigDigest: digest}, {VCPUCount: 1, CPUConfigDigest: digest}},
		{{VCPUCount: 1, CPUConfigDigest: "sha256:invalid"}, {VCPUCount: 2, CPUConfigDigest: digest}},
	} {
		if err := ValidateCPUShapeEvidence(invalid, 2); err == nil {
			t.Fatalf("shape evidence %+v validated", invalid)
		}
	}
}

func TestHostRuntimeEvidenceStoreBindsIdentityAndImmutableCPUMapOnce(t *testing.T) {
	evidence := testHostRuntimeEvidence(t, 2, testProbeRuntimeArtifacts())
	identical := evidence
	identical.CPUShapes = append([]CPUShapeEvidence(nil), evidence.CPUShapes...)
	tampered := evidence
	tampered.RuntimeID = testCanonicalDigest("f")
	if _, err := tampered.RuntimeIdentity(); err == nil || !strings.Contains(err.Error(), "does not match canonical ID") {
		t.Fatalf("tampered runtime identity error = %v", err)
	}
	firstDigest := evidence.CPUShapes[0].CPUConfigDigest
	secondDigest := evidence.CPUShapes[1].CPUConfigDigest
	store := newHostRuntimeEvidenceStore()
	if _, err := store.runtimeIdentity(); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("unbound identity error = %v", err)
	}
	if _, err := store.cpuConfigDigest(1); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("unbound digest error = %v", err)
	}
	if err := store.bind(evidence, 2); err != nil {
		t.Fatal(err)
	}
	evidence.CPUShapes[0].CPUConfigDigest = testCanonicalDigest("c")
	if got, err := store.cpuConfigDigest(1); err != nil || got != firstDigest {
		t.Fatalf("digest(1) = %q, %v; want immutable %q", got, err, firstDigest)
	}
	if got, err := store.cpuConfigDigest(2); err != nil || got != secondDigest {
		t.Fatalf("digest(2) = %q, %v; want %q", got, err, secondDigest)
	}
	if _, err := store.cpuConfigDigest(3); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("digest(3) error = %v", err)
	}
	boundIdentity, err := store.runtimeIdentity()
	if err != nil || boundIdentity.ID == "" {
		t.Fatalf("bound identity = %+v, %v", boundIdentity, err)
	}
	if err := store.bind(identical, 2); err != nil {
		t.Fatalf("identical rebind: %v", err)
	}
	changed := identical
	changed.HostKernelRelease = "6.12.32"
	changed.RuntimeID = mustDeriveTestRuntimeIdentity(t, changed).ID
	if err := store.bind(changed, 2); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed rebind error = %v", err)
	}
}

func TestPinnedRuntimeExecutableRetainsMeasuredBytesAndFailsClosedOnMutation(t *testing.T) {
	sourceBody := []byte("measured Firecracker executable")
	sourcePath := writeProbeTestFile(t, t.TempDir(), "firecracker", string(sourceBody))
	expectedDigest := sha256sum.DigestBytes(sourceBody)
	pinnedPath, err := pinRuntimeExecutable(
		sourcePath,
		filepath.Join(t.TempDir(), "vms", "guest"),
		expectedDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if pinnedPath == sourcePath || filepath.Base(pinnedPath) != "firecracker" {
		t.Fatalf("pinned path = %q, source = %q", pinnedPath, sourcePath)
	}
	if digest, err := digestFile(pinnedPath); err != nil || digest != expectedDigest {
		t.Fatalf("pinned digest = %q, %v; want %q", digest, err, expectedDigest)
	}
	if err := os.WriteFile(sourcePath, []byte("replacement executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if digest, err := digestFile(pinnedPath); err != nil || digest != expectedDigest {
		t.Fatalf("pinned digest after source replacement = %q, %v; want %q", digest, err, expectedDigest)
	}

	evidence := testHostRuntimeEvidence(t, 1, testProbeRuntimeArtifacts())
	evidence.FirecrackerDigest = expectedDigest
	evidence.firecrackerPath = pinnedPath
	evidence.RuntimeID = mustDeriveTestRuntimeIdentity(t, evidence).ID
	store := newHostRuntimeEvidenceStore()
	if err := store.bind(evidence, 1); err != nil {
		t.Fatal(err)
	}
	if path, err := store.firecrackerExecutable(); err != nil || path != pinnedPath {
		t.Fatalf("bound executable = %q, %v; want %q", path, err, pinnedPath)
	}
	if err := os.Chmod(pinnedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pinnedPath, []byte("mutated pinned executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pinnedPath, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := store.firecrackerExecutable(); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("mutated pinned executable error = %v", err)
	}
}

func flagValue(t *testing.T, arguments []string, flag string) string {
	t.Helper()
	index := slices.Index(arguments, flag)
	if index < 0 || index+1 >= len(arguments) {
		t.Fatalf("arguments %v have no %s value", arguments, flag)
	}
	return arguments[index+1]
}

func writeProbeTestFile(t *testing.T, directory string, name string, body string) string {
	t.Helper()
	path := directory + "/" + name
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func testCanonicalDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}

func testProbeRuntimeArtifacts() runtimeArtifacts {
	return runtimeArtifacts{
		Arch:              "amd64",
		VMRuntimeContract: runtimeid.Contract,
		Kernel:            runtimeArtifact{Digest: testCanonicalDigest("1")},
		Initramfs:         runtimeArtifact{Digest: testCanonicalDigest("2")},
		Rootfs:            runtimeArtifact{Digest: testCanonicalDigest("3")},
	}
}

func testCPUShapes(maxVCPUCount int64) []CPUShapeEvidence {
	shapes := make([]CPUShapeEvidence, 0, maxVCPUCount)
	for vcpuCount := int64(1); vcpuCount <= maxVCPUCount; vcpuCount++ {
		shapes = append(shapes, CPUShapeEvidence{
			VCPUCount:       vcpuCount,
			CPUConfigDigest: testCPUConfigDigest(vcpuCount),
		})
	}
	return shapes
}

func testCPUConfigDigest(vcpuCount int64) string {
	return sha256sum.DigestBytes([]byte(fmt.Sprintf("test CPU config for %d vCPUs", vcpuCount)))
}

func testHostRuntimeEvidence(t *testing.T, maxVCPUCount int64, artifacts runtimeArtifacts) HostRuntimeEvidence {
	t.Helper()
	descriptorDigest, err := CanonicalVMRuntimeDescriptor().Digest()
	if err != nil {
		t.Fatal(err)
	}
	architecture, err := runtimeid.ArchitectureFromGo(artifacts.Arch)
	if err != nil {
		t.Fatal(err)
	}
	firecrackerBody := []byte("test Firecracker executable")
	firecrackerPath := writeProbeTestFile(t, t.TempDir(), "firecracker", string(firecrackerBody))
	if err := os.Chmod(firecrackerPath, 0o500); err != nil {
		t.Fatal(err)
	}
	evidence := HostRuntimeEvidence{
		RuntimeArch:               architecture,
		VMRuntimeContract:         artifacts.VMRuntimeContract,
		FirecrackerDigest:         sha256sum.DigestBytes(firecrackerBody),
		FirecrackerVersion:        "1.16.1",
		SnapshotFormatVersion:     "5.0.0",
		CPUTemplateHelperDigest:   testCanonicalDigest("5"),
		HostKernelRelease:         "6.12.31",
		VMRuntimeDescriptorDigest: descriptorDigest,
		CPUTemplateSelector:       NoCPUTemplateSelector(),
		CPUShapes:                 testCPUShapes(maxVCPUCount),
		KernelDigest:              artifacts.Kernel.Digest,
		InitramfsDigest:           artifacts.Initramfs.Digest,
		RootfsDigest:              artifacts.Rootfs.Digest,
		firecrackerPath:           firecrackerPath,
	}
	evidence.RuntimeID = mustDeriveTestRuntimeIdentity(t, evidence).ID
	return evidence
}

func mustDeriveTestRuntimeIdentity(t *testing.T, evidence HostRuntimeEvidence) runtimeid.Selector {
	t.Helper()
	identity, err := deriveHostRuntimeIdentity(evidence)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
