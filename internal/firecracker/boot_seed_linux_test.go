//go:build linux

package firecracker

import (
	"bytes"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestBootSeedMatchesContentBeforeStartingAnotherComputer(t *testing.T) {
	digest := func(s string) string { return "sha256:" + strings.Repeat(s, 64) }
	c := &Connector{cfg: Config{NetworkResolverIPv4: "1.1.1.1", GuestPort: 5000, HealthPort: 5001}}
	root := digest("1")
	original := vm.MaterializeRequest{NetworkPhase: networkpolicy.PhaseRuntime, NetworkPolicy: networkpolicy.OrdinaryPolicy(), NetworkInstallation: networkpolicy.Installation{}.Canonical(),
		ID:                        "private-builder",
		Binding:                   vm.WorkloadBinding{VMPlatformID: digest("2")},
		BaseComputerDiskVersionID: root,
		Resources:                 vm.Resources{MilliCPU: 2000, MemoryMiB: 2048, DiskMiB: 4096, Slots: 1},
		VMVCPUCount:               2, CPUConfigDigest: digest("3"),
		Topology:       vm.Topology{Computer: &vm.ComputerDisk{ComputerID: "private-builder", Device: &ownedComputerFixture{}, VersionID: root, SizeBytes: 1 << 30}},
		ReadOnlyDrives: testProgramDrives(&recordingReadOnlyDriveSource{}),
	}
	manifest, err := c.bootSeedManifest(original)
	if err != nil {
		t.Fatal(err)
	}
	clone := original
	clone.ID = "new-computer-instance"
	clone.Topology.Computer = &vm.ComputerDisk{ComputerID: "new-computer", Device: &ownedComputerFixture{}, VersionID: root, SizeBytes: 1 << 30}
	fresh, err := c.bootSeedManifest(clone)
	if err != nil || !bytes.Equal(manifest, fresh) || bytes.Contains(manifest, []byte("private-builder")) {
		t.Fatalf("seed retained source identity: %v", err)
	}
	for _, change := range []struct {
		name string
		edit func(*vm.MaterializeRequest, *Connector)
	}{
		{"root", func(r *vm.MaterializeRequest, _ *Connector) {
			r.Topology.Computer.VersionID = digest("4")
			r.BaseComputerDiskVersionID = digest("4")
		}},
		{"disk-size", func(r *vm.MaterializeRequest, _ *Connector) { r.Topology.Computer.SizeBytes += 4096 }},
		{"program", func(r *vm.MaterializeRequest, _ *Connector) { r.ReadOnlyDrives[1].Digest = digest("5") }},
		{"runtime-program", func(r *vm.MaterializeRequest, _ *Connector) { r.ReadOnlyDrives[0].Digest = digest("5") }},
		{"program-absent", func(r *vm.MaterializeRequest, _ *Connector) { r.ReadOnlyDrives = nil }},
		{"platform", func(r *vm.MaterializeRequest, _ *Connector) { r.Binding.VMPlatformID = digest("6") }},
		{"cpu", func(r *vm.MaterializeRequest, _ *Connector) { r.CPUConfigDigest = digest("7") }},
		{"vcpus", func(r *vm.MaterializeRequest, _ *Connector) { r.VMVCPUCount++ }},
		{"memory", func(r *vm.MaterializeRequest, _ *Connector) { r.Resources.MemoryMiB++ }},
		{"scratch", func(r *vm.MaterializeRequest, _ *Connector) { r.Resources.DiskMiB++ }},
		{"resolver", func(_ *vm.MaterializeRequest, c *Connector) { c.cfg.NetworkResolverIPv4 = "8.8.8.8" }},
		{"guest-port", func(_ *vm.MaterializeRequest, c *Connector) { c.cfg.GuestPort++ }},
	} {
		t.Run(change.name, func(t *testing.T) {
			r, connector := clone, *c
			computer := *clone.Topology.Computer
			r.Topology.Computer = &computer
			r.ReadOnlyDrives = append([]vm.ReadOnlyDrive(nil), clone.ReadOnlyDrives...)
			change.edit(&r, &connector)
			r.BootSeed = &vm.BootSeedRestore{Manifest: manifest}
			_, err := connector.restoreBootSeed(t.Context(), r)
			if err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("incompatible seed reached physical startup: %v", err)
			}
		})
	}
}
