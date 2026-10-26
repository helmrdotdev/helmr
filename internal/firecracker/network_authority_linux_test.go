//go:build linux

package firecracker

import (
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/vm"
	"net/netip"
	"strings"
	"testing"
)

func TestAllocationNetworkAuthorityFailsClosedAndIntersectsHost(t *testing.T) {
	control := networkpolicy.Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.0.0/16")}, BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("10.90.0.0/16")}}
	host := &Connector{cfg: Config{NetworkPrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.0.0/17")}, NetworkBlockedIPv4CIDRs: control.BlockedIPv4CIDRs}}
	policy := networkpolicy.Policy{Kind: networkpolicy.Allowlist, Domains: []networkpolicy.Domain{{Domain: "private.example.com", IPRanges: []string{"10.60.0.0/16"}}}}
	cfg, err := host.configForNetwork(policy, control)
	if err != nil {
		t.Fatal("compatible stricter host rejected", err)
	}
	if cfg.NetworkPolicy.Digest() != policy.Digest() || cfg.NetworkInstallation.Digest() != control.Digest() {
		t.Fatal("authenticated maximum changed")
	}
	if _, err := host.configForNetwork(networkpolicy.Policy{}, control); err == nil {
		t.Fatal("missing authority accepted")
	}
	host.cfg.NetworkPrivateBusinessCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if _, err := host.configForNetwork(policy, control); err == nil {
		t.Fatal("permissive host accepted")
	}
}
func TestCheckpointPolicyIdentityCannotChange(t *testing.T) {
	cfg := Config{NetworkResolverIPv4: "1.1.1.1", NetworkPolicy: networkpolicy.OrdinaryPolicy()}
	old := snapshotNetworkConfig(cfg)
	cfg.NetworkPolicy = networkpolicy.BlockedPolicy()
	if validateRestoredNetworkConfig(snapshotNetworkConfig(cfg), old) == nil {
		t.Fatal("checkpoint switched fixed policy")
	}
	old.PolicyDigest = ""
	if validateSnapshotNetwork(old) == nil {
		t.Fatal("unqualified checkpoint accepted")
	}
}

func TestExistingPolicyIntersectsNarrowerInstallationOnLaunchAndRestore(t *testing.T) {
	for _, tc := range []struct {
		name, rangeCIDR   string
		previous, current networkpolicy.Installation
		allowed, denied   string
	}{
		{name: "narrow private ceiling and add management deny", rangeCIDR: "10.60.0.0/16", previous: networkpolicy.Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.0.0/16")}}, current: networkpolicy.Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.1.0/24")}, BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.2.0/24")}}, allowed: "10.60.1.7", denied: "10.60.2.7"},
		{name: "add public deny", rangeCIDR: "8.8.8.0/24", previous: networkpolicy.Installation{}, current: networkpolicy.Installation{BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("8.8.8.128/25")}}, allowed: "8.8.8.7", denied: "8.8.8.137"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := networkpolicy.Policy{Kind: networkpolicy.Allowlist, Domains: []networkpolicy.Domain{{Domain: "api.example.com", IPRanges: []string{tc.rangeCIDR}}}}
			if err := policy.ValidateInstallation(tc.previous); err != nil {
				t.Fatal("original publication invalid", err)
			}
			if err := policy.ValidateInstallation(tc.current); err == nil {
				t.Fatal("fixture must no longer satisfy current publication containment")
			}
			previous := tc.previous.Canonical()
			current := tc.current.Canonical()
			host := &Connector{cfg: Config{NetworkResolverIPv4: "1.1.1.1", NetworkPrivateBusinessCIDRs: current.PrivateBusinessCIDRs, NetworkBlockedIPv4CIDRs: current.BlockedIPv4CIDRs}}
			cfg, err := host.configForNetwork(policy, current)
			if err != nil {
				t.Fatal("old immutable pin must execute under current intersection", err)
			}
			captured := cfg
			captured.NetworkInstallation = previous
			if err := validateRestoredNetworkConfig(snapshotNetworkConfig(cfg), snapshotNetworkConfig(captured)); err != nil {
				t.Fatal("immutable policy restore rejected stricter installation", err)
			}
			if !policy.AllowsAddress("api.example.com", netip.MustParseAddr(tc.allowed), cfg.NetworkInstallation) || policy.AllowsAddress("api.example.com", netip.MustParseAddr(tc.denied), cfg.NetworkInstallation) {
				t.Fatal("execution did not intersect current restrictions")
			}
		})
	}
}

func TestMissingAllocationInstallationAuthorityCannotExecute(t *testing.T) {
	c := &Connector{}
	if _, err := c.configForNetwork(networkpolicy.OrdinaryPolicy(), networkpolicy.Installation{}); err == nil {
		t.Fatal("missing whole DTO installation accepted")
	}
	if _, err := c.configForNetwork(networkpolicy.OrdinaryPolicy(), networkpolicy.Installation{}.Canonical()); err != nil {
		t.Fatal("explicit empty authority rejected", err)
	}
}

func TestStartupProbeProducerSuppliesBlockedNetworkAuthority(t *testing.T) {
	c := &Connector{cfg: Config{NetworkPrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.0.0/16")}, NetworkBlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("10.90.0.0/16")}}}
	request := c.startupProbeRequest("sha256:" + strings.Repeat("1", 64))
	if request.NetworkPolicy.Kind != networkpolicy.Blocked {
		t.Fatal("startup probe acquired external authority")
	}
	if request.NetworkInstallation.PrivateBusinessCIDRs == nil || request.NetworkInstallation.BlockedIPv4CIDRs == nil {
		t.Fatal("startup probe omitted installation authority")
	}
	if err := request.Binding.Validate(vm.Owner{Kind: request.OwnerKind, ID: request.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.connectorForRequest(request); err != nil {
		t.Fatal("actual startup producer request cannot pass normal launch admission", err)
	}
}

func TestMaterializationRequiresKnownSourcePhase(t *testing.T) {
	c := &Connector{cfg: Config{VCPUCount: 2, MemoryMiB: 1024}}
	resources := vm.Resources{MilliCPU: 1000, MemoryMiB: 512, Slots: 1}
	if _, err := c.configForMaterializeRequest(vm.MaterializeRequest{Resources: resources, NetworkPolicy: networkpolicy.OrdinaryPolicy(), NetworkInstallation: networkpolicy.Installation{}.Canonical()}); err == nil {
		t.Fatal("unknown execution phase silently defaulted to runtime")
	}
	for _, phase := range []networkpolicy.Phase{networkpolicy.PhaseRuntime, networkpolicy.PhasePreparation, networkpolicy.PhaseBootSeed, networkpolicy.PhaseQualification} {
		cfg, err := c.configForMaterializeRequest(vm.MaterializeRequest{Resources: resources, NetworkPhase: phase, NetworkPolicy: networkpolicy.BlockedPolicy(), NetworkInstallation: networkpolicy.Installation{}.Canonical(), Topology: vm.Topology{Computer: &vm.ComputerDisk{ComputerID: "source-owner"}}})
		if err != nil {
			t.Fatal(phase, err)
		}
		if cfg.NetworkPhase != phase {
			t.Fatal("source phase changed")
		}
		if phase == networkpolicy.PhaseRuntime && cfg.NetworkComputerID != "source-owner" {
			t.Fatal("runtime actual Computer lost")
		}
		if phase != networkpolicy.PhaseRuntime && cfg.NetworkComputerID != "" {
			t.Fatal("preparation owner became a logical Computer")
		}
	}
}
