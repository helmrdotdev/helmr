//go:build linux

package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/vm"
)

const dependencyFirecrackerManagerPath = "/opt/helmr/manager/bin/bun"

func init() {
	if os.Args[0] != dependencyFirecrackerManagerPath || os.Geteuid() != 65532 {
		return
	}
	switch {
	case len(os.Args) == 2 && os.Args[1] == "--version":
		_, _ = os.Stdout.WriteString("1.3.10\n")
		os.Exit(0)
	case len(os.Args) == 3 && os.Args[1] == "install" && os.Args[2] == "--help":
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func TestDependencyManagerFirecrackerBoundary(t *testing.T) {
	if os.Getenv("HELMR_FIRECRACKER_DEPENDENCY_TEST") != "1" {
		t.Skip("set HELMR_FIRECRACKER_DEPENDENCY_TEST=1 on a disposable KVM host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Firecracker dependency test requires root")
	}
	architecture, asset := dependencyFirecrackerArchitecture(t)
	artifacts := os.Getenv("HELMR_FIRECRACKER_TEST_ARTIFACTS")
	if artifacts == "" {
		t.Fatal("HELMR_FIRECRACKER_TEST_ARTIFACTS is required")
	}

	root, err := os.MkdirTemp("", "hfc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove Firecracker test directory: %v", err)
		}
	})
	managerTree := dependencyFirecrackerManagerTree(t, root)
	runtimeTree := dependencyFirecrackerTree(t, root, "runtime", nil)
	toolchainTree := dependencyFirecrackerToolchainTree(t, root, architecture)

	managerArtifact := dependencyFirecrackerArtifact(
		t,
		managerTree,
		deployment.ManagerTreeMediaType,
	)
	runtimeArtifact := dependencyFirecrackerArtifact(
		t,
		runtimeTree,
		deployment.RuntimeArtifactMediaType,
	)
	toolchainArtifact := dependencyFirecrackerArtifact(
		t,
		toolchainTree,
		deployment.ToolchainMediaType,
	)
	manager := deployment.PackageManager{
		Name:    deployment.PackageManagerBun,
		Version: "1.3.10",
	}
	capsule := deployment.ManagerCapsule{
		Architecture: architecture,
		Entrypoint: deployment.ManagerEntrypoint{
			Kind: deployment.ManagerEntrypointNative,
			Path: dependencyFirecrackerManagerPath,
		},
		FormatVersion:  deployment.ManagerCapsuleFormatVersion,
		PackageManager: manager,
		Source: deployment.ManagerSource{
			Digest: "sha256:" + strings.Repeat("1", 64),
			Origin: "https://github.com/oven-sh/bun/releases/download/bun-v" +
				manager.Version + "/" + asset,
			SizeBytes: 1,
		},
		Tree: managerArtifact,
	}
	toolchain := deployment.Toolchain{
		Architecture:         architecture,
		FormatVersion:        deployment.ToolchainFormatVersion,
		ManagedRuntimeDigest: runtimeArtifact.Digest,
		ToolchainClosure:     toolchainArtifact,
	}
	plan, err := deployment.NewDependencyPlan(
		capsule,
		toolchain,
		deployment.DependencyMaterializerVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	planDigest, err := deployment.DependencyPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	request := deployment.ManagerRequest{
		DependencyPlan:       plan,
		DependencyPlanDigest: planDigest,
		FormatVersion:        deployment.ManagerFormatVersion,
		ManagerCapsule:       capsule,
		ManagerTree:          managerArtifact,
		Operation:            deployment.ManagerProbe,
		Runtime:              runtimeArtifact,
		StandardToolchain:    toolchainArtifact,
	}

	jailerRoot := filepath.Join(root, "jailer")
	if err := os.MkdirAll(jailerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	stagedArtifacts, err := PrepareRuntime(artifacts, root, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	connector, err := NewConnector(Config{
		FirecrackerPath:         dependencyFirecrackerExecutable(t, "firecracker"),
		JailerPath:              dependencyFirecrackerExecutable(t, "jailer"),
		JailerUID:               1000,
		JailerGID:               1000,
		JailerChrootBaseDir:     jailerRoot,
		KernelPath:              filepath.Join(stagedArtifacts, "vmlinuz"),
		InitramfsPath:           filepath.Join(stagedArtifacts, "initramfs"),
		RootfsPath:              filepath.Join(stagedArtifacts, "rootfs.ext4"),
		RuntimeArtifactsPath:    filepath.Join(stagedArtifacts, "runtime-artifacts.json"),
		StateDir:                filepath.Join(root, "state"),
		ScratchDiskMiB:          compute.BuildGuestResources().DiskMiB,
		NetworkBlockedIPv4CIDRs: []string{},
		NetworkBlockedIPv6CIDRs: []string{},
		HealthTimeout:           time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		t.Run(fmt.Sprintf("launch-%d", attempt), func(t *testing.T) {
			runDependencyFirecrackerProbe(
				t,
				connector,
				request,
				root,
				manager,
				attempt,
				managerTree,
				runtimeTree,
				toolchainTree,
			)
		})
	}
}

func runDependencyFirecrackerProbe(
	t *testing.T,
	connector *Connector,
	request deployment.ManagerRequest,
	root string,
	manager deployment.PackageManager,
	attempt int,
	managerTree string,
	runtimeTree string,
	toolchainTree string,
) {
	t.Helper()
	session, err := connector.Connect(context.Background(), vm.ConnectRequest{
		OwnerKind:   vm.OwnerBuild,
		Resources:   compute.BuildGuestResources(),
		PIDsMax:     compute.DependencyGuestPIDsMax,
		Networkless: true,
		BuildDrives: []vm.ReadOnlyDrive{
			{ID: vm.ManagerDrive, Source: dependencyFirecrackerDrive{managerTree}},
			{ID: vm.ManagedRuntimeDrive, Source: dependencyFirecrackerDrive{runtimeTree}},
			{ID: vm.ToolchainDrive, Source: dependencyFirecrackerDrive{toolchainTree}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := session.Close(closeContext); err != nil {
			t.Errorf("close Firecracker dependency session: %v", err)
		}
	}()

	stream := session.Stream()
	requestContext, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := writeDependencyFirecrackerRequest(stream, request); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	metadata, tree, err := deployment.ReadManagerResponse(
		requestContext,
		stream,
		filepath.Join(root, fmt.Sprintf("response-%d", attempt)),
		request,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if tree != nil {
		t.Fatal("manager probe returned a tree")
	}
	if metadata.Outcome != deployment.ManagerSucceeded ||
		metadata.ObservedVersion == nil ||
		*metadata.ObservedVersion != manager.Version {
		t.Fatalf("manager probe metadata = %#v", metadata)
	}
}

func writeDependencyFirecrackerRequest(
	destination io.Writer,
	request deployment.ManagerRequest,
) error {
	body, err := deployment.CanonicalManagerRequest(request)
	if err != nil {
		return err
	}
	if err := frameio.WriteStreamFrameHeader(
		destination,
		[]byte(`{"type":"dependency-manager"}`),
		uint64(len(body)),
	); err != nil {
		return err
	}
	_, err = destination.Write(body)
	return err
}

type dependencyFirecrackerDrive struct {
	path string
}

func (drive dependencyFirecrackerDrive) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	if drive.path == "" {
		return errors.New("dependency test drive path is empty")
	}
	target := filepath.Join(directory, name)
	if err := os.Link(drive.path, target); err != nil {
		return err
	}
	if err := os.Chown(target, uid, gid); err != nil {
		return err
	}
	return os.Chmod(target, 0o400)
}

func dependencyFirecrackerArchitecture(
	t *testing.T,
) (deployment.RuntimeArchitecture, string) {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64":
		return deployment.ArchitectureX8664, "bun-linux-x64-baseline.zip"
	case "arm64":
		return deployment.ArchitectureAArch64, "bun-linux-aarch64.zip"
	default:
		t.Fatalf("unsupported Firecracker test architecture %q", runtime.GOARCH)
		return "", ""
	}
}

func dependencyFirecrackerManagerTree(t *testing.T, root string) string {
	t.Helper()
	source, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	return dependencyFirecrackerTree(t, root, "manager", func(directory string) {
		target := filepath.Join(directory, "bin", "bun")
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		destination, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(destination, source)
		closeErr := destination.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatal(errors.Join(copyErr, closeErr))
		}
	})
}

func dependencyFirecrackerToolchainTree(
	t *testing.T,
	root string,
	architecture deployment.RuntimeArchitecture,
) string {
	t.Helper()
	loader := "helmr/manager/lib/ld-linux-aarch64.so.1"
	if architecture == deployment.ArchitectureX8664 {
		loader = "helmr/manager/lib/ld-linux-x86-64.so.2"
	}
	return dependencyFirecrackerTree(t, root, "toolchain", func(directory string) {
		for _, relative := range []string{"bin/env", "bin/sh", loader} {
			path := filepath.Join(directory, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("fixture"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func dependencyFirecrackerTree(
	t *testing.T,
	root string,
	name string,
	populate func(string),
) string {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if populate != nil {
		populate(directory)
	}
	image := filepath.Join(root, name+".squashfs")
	command := exec.Command(
		dependencyFirecrackerExecutable(t, "mksquashfs"),
		directory,
		image,
		"-noappend",
		"-no-progress",
		"-no-xattrs",
		"-all-root",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create %s test drive: %v: %s", name, err, output)
	}
	return image
}

func dependencyFirecrackerArtifact(
	t *testing.T,
	path string,
	mediaType string,
) deployment.ManagerArtifact {
	t.Helper()
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, source)
	if err != nil {
		t.Fatal(err)
	}
	return deployment.ManagerArtifact{
		Digest:    "sha256:" + hex.EncodeToString(hash.Sum(nil)),
		MediaType: mediaType,
		SizeBytes: size,
	}
}

func dependencyFirecrackerExecutable(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("locate %s: %v", name, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
