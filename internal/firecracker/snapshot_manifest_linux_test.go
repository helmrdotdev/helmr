//go:build linux

package firecracker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestSnapshotRuntimeConfigIncludesNetworkTopology(t *testing.T) {
	cfg := (Config{NetworkResolverIPv4: "10.0.0.2"}).WithDefaults()
	runtimeID := testVMPlatform(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333").ID
	digest, manifestBytes, err := snapshotRuntimeConfig(cfg, "checkpoint-1", runtimeID, testCPUConfigDigest(cfg.VCPUCount), "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333", runtimeKernelArgs(vm.Topology{}, nil, cfg.NetworkResolverIPv4), vm.Topology{})
	if err != nil {
		t.Fatal(err)
	}
	if digest != sha256sum.DigestBytes(manifestBytes) {
		t.Fatalf("digest = %q, want %q", digest, sha256sum.DigestBytes(manifestBytes))
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	network := manifest.RuntimeState.Network
	if network.GuestIPv4CIDR != GuestNetworkCIDRV0 || network.GuestMAC != GuestMACV0 || network.GatewayIPv4 != GuestGatewayIPv4V0 || network.GatewayMAC != GuestGatewayMACV0 || network.GuestInterfaceName != GuestInterfaceNameV0 || network.MTU != GuestMTUV0 || len(network.ResolverAddresses) != 1 || network.ResolverAddresses[0] != "10.0.0.2" {
		t.Fatalf("network = %+v", network)
	}
	if manifest.RecoveryPoint.Runtime.ID != runtimeID || manifest.RecoveryPoint.Runtime.InitramfsDigest != "sha256:2222222222222222222222222222222222222222222222222222222222222222" {
		t.Fatalf("runtime = %+v", manifest.RecoveryPoint.Runtime)
	}
	descriptorDigest, err := CanonicalVMRuntimeDescriptor().Digest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RecoveryPoint.Runtime.DescriptorDigest != descriptorDigest {
		t.Fatalf("runtime descriptor digest = %q, want %q", manifest.RecoveryPoint.Runtime.DescriptorDigest, descriptorDigest)
	}
	if manifest.RecoveryPoint.Runtime.CPUConfigDigest != testCPUConfigDigest(cfg.VCPUCount) {
		t.Fatalf("runtime CPU config digest = %q", manifest.RecoveryPoint.Runtime.CPUConfigDigest)
	}
}

func TestSnapshotRuntimeConfigBindsManagedProgramTopology(t *testing.T) {
	cfg := (Config{NetworkResolverIPv4: "10.0.0.2"}).WithDefaults()
	runtimeID := testVMPlatform(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333").ID
	drives := testProgramDrives(&recordingReadOnlyDriveSource{})
	_, manifestBytes, err := snapshotRuntimeConfig(
		cfg, "checkpoint-1", runtimeID, testCPUConfigDigest(cfg.VCPUCount), "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333", runtimeKernelArgs(vm.Topology{}, drives, cfg.NetworkResolverIPv4), vm.Topology{}, drives,
	)
	if err != nil {
		t.Fatal(err)
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.RecoveryPoint.Runtime.KernelArgs != runtimeKernelArgs(vm.Topology{}, drives, cfg.NetworkResolverIPv4) {
		t.Fatalf("kernel args = %q", manifest.RecoveryPoint.Runtime.KernelArgs)
	}
	program := manifest.RecoveryPoint.Runtime.Program
	if program == nil ||
		program.Runtime.Digest != drives[0].Digest ||
		program.Artifact.Digest != drives[1].Digest {
		t.Fatalf("Program = %+v", program)
	}
	if err := validateSnapshotProgram(program, drives); err != nil {
		t.Fatal(err)
	}
	mismatch := append([]vm.ReadOnlyDrive(nil), drives...)
	mismatch[1].Digest = "sha256:" + strings.Repeat("4", 64)
	if err := validateSnapshotProgram(program, mismatch); err == nil {
		t.Fatal("mismatched Program was accepted")
	}
}

func TestSnapshotRuntimeConfigRequiresResolver(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	runtimeID := testVMPlatform(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333").ID
	_, _, err := snapshotRuntimeConfig(cfg, "checkpoint-1", runtimeID, testCPUConfigDigest(cfg.VCPUCount), "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333", runtimeKernelArgs(vm.Topology{}, nil, cfg.NetworkResolverIPv4), vm.Topology{})
	if err == nil {
		t.Fatal("expected missing resolver error")
	}
}

func TestValidateRestoredNetworkConfigRequiresExactGuestVisibleIdentity(t *testing.T) {
	expected := snapshotNetworkConfig(Config{NetworkResolverIPv4: "10.0.0.2"})
	if err := validateRestoredNetworkConfig(expected, expected); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(*snapshotNetworkManifest)
	}{
		{name: "guest IP", edit: func(network *snapshotNetworkManifest) {
			network.GuestIPv4CIDR = "192.168.127.3/30"
		}},
		{name: "guest MAC", edit: func(network *snapshotNetworkManifest) {
			network.GuestMAC = "06:00:ac:10:00:03"
		}},
		{name: "gateway", edit: func(network *snapshotNetworkManifest) {
			network.GatewayIPv4 = "192.168.127.254"
		}},
		{name: "gateway MAC", edit: func(network *snapshotNetworkManifest) {
			network.GatewayMAC = "02:fc:00:00:00:03"
		}},
		{name: "resolver", edit: func(network *snapshotNetworkManifest) {
			network.ResolverAddresses = []string{"10.0.0.3"}
		}},
		{name: "guest interface", edit: func(network *snapshotNetworkManifest) {
			network.GuestInterfaceName = "eth1"
		}},
		{name: "mtu", edit: func(network *snapshotNetworkManifest) {
			network.MTU++
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := expected
			actual.ResolverAddresses = append([]string(nil), expected.ResolverAddresses...)
			test.edit(&actual)
			if err := validateRestoredNetworkConfig(expected, actual); err == nil {
				t.Fatal("expected network mismatch")
			}
		})
	}
}
