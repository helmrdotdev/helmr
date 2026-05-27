package firecracker

import (
	"strings"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	if cfg.FirecrackerPath != DefaultFirecrackerPath || cfg.VCPUCount != DefaultVCPUs || cfg.MemoryMiB != DefaultMemoryMiB {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.JailerPath != DefaultJailerPath || cfg.JailerChrootBaseDir == "" || cfg.CgroupVersion != DefaultCgroupVersion {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.CNINetworkName != DefaultCNINetworkName || cfg.CNIConfDir != DefaultCNIConfDir || cfg.CNIBinDir != DefaultCNIBinDir || cfg.CNIIfName != DefaultCNIIfName || cfg.CNIVMIfName != DefaultCNIVMIfName {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.CNIProfile != "helmr/v0" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.GuestPort != DefaultGuestPort || cfg.HealthPort != HealthPort || cfg.StateDir == "" || cfg.HealthTimeout == 0 {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.NetworkBlockedIPv4CIDRs != nil || cfg.NetworkBlockedIPv6CIDRs != nil {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestConfigValidateNetworkBlockedCIDRs(t *testing.T) {
	cfg := (Config{
		NetworkBlockedIPv4CIDRs: []string{"fc00::/7"},
		NetworkBlockedIPv6CIDRs: []string{"10.0.0.0/8"},
	}).WithDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	text := err.Error()
	for _, want := range []string{`"fc00::/7" must be IPv4`, `"10.0.0.0/8" must be IPv6`} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q does not contain %q", text, want)
		}
	}
}

func TestConfigValidateRequiresNetworkPolicy(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	text := err.Error()
	for _, want := range []string{"firecracker network blocked IPv4 CIDRs are required", "firecracker network blocked IPv6 CIDRs are required"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q does not contain %q", text, want)
		}
	}
}

func TestConfigValidateAllowsExplicitEmptyNetworkPolicy(t *testing.T) {
	cfg := (Config{
		NetworkBlockedIPv4CIDRs: []string{},
		NetworkBlockedIPv6CIDRs: []string{},
	}).WithDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected boot input validation errors")
	}
	text := err.Error()
	for _, unexpected := range []string{"firecracker network blocked IPv4 CIDRs are required", "firecracker network blocked IPv6 CIDRs are required"} {
		if strings.Contains(text, unexpected) {
			t.Fatalf("error %q contains %q", text, unexpected)
		}
	}
}

func TestConfigValidateRequiresBootInputs(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	cfg.CNINetworkName = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	text := err.Error()
	for _, want := range []string{"firecracker jailer uid", "firecracker jailer gid", "guest kernel path", "guest initramfs path", "guest rootfs path", "guest CNI network name"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q does not contain %q", text, want)
		}
	}
}
