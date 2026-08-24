package firecracker

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	if cfg.FirecrackerPath != DefaultFirecrackerPath || cfg.CPUTemplateHelperPath != DefaultCPUTemplateHelperPath || cfg.VCPUCount != DefaultVCPUs || cfg.MemoryMiB != DefaultMemoryMiB {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.CPUTemplateSelector != NoCPUTemplateSelector() {
		t.Fatalf("CPU template selector = %+v", cfg.CPUTemplateSelector)
	}
	if cfg.JailerPath != DefaultJailerPath || cfg.JailerChrootBaseDir == "" || cfg.CgroupVersion != DefaultCgroupVersion {
		t.Fatalf("config = %+v", cfg)
	}
	if filepath.Dir(cfg.StateDir) != filepath.Dir(cfg.JailerChrootBaseDir) || pathsOverlap(cfg.StateDir, cfg.JailerChrootBaseDir) {
		t.Fatalf("state and jailer directories are not disjoint siblings: state=%q jailer=%q", cfg.StateDir, cfg.JailerChrootBaseDir)
	}
	if cfg.GuestPort != DefaultGuestPort || cfg.HealthPort != HealthPort || cfg.StateDir == "" || cfg.InitTimeout != DefaultInitTimeout || cfg.HealthTimeout != DefaultHealthTimeout || cfg.HealthAttemptTimeout != DefaultHealthAttemptTimeout {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestConfigValidateRejectsInvalidCPUTemplateConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "unknown selector", cfg: Config{CPUTemplateSelector: CPUTemplateSelector{Kind: "static"}}, want: "selector kind"},
		{name: "none with digest", cfg: Config{CPUTemplateSelector: CPUTemplateSelector{Kind: CPUTemplateNone, Digest: "sha256:" + strings.Repeat("a", 64)}}, want: "must not include a digest"},
		{name: "custom digest", cfg: Config{CPUTemplateSelector: CPUTemplateSelector{Kind: CPUTemplateCustom, Digest: "sha256:invalid"}, CustomCPUTemplatePath: "/template.json"}, want: "canonical SHA-256"},
		{name: "custom path missing", cfg: Config{CPUTemplateSelector: CPUTemplateSelector{Kind: CPUTemplateCustom, Digest: "sha256:" + strings.Repeat("a", 64)}}, want: "custom CPU template path is required"},
		{name: "none with path", cfg: Config{CustomCPUTemplatePath: "/template.json"}, want: "path requires the custom"},
		{name: "none with whitespace path", cfg: Config{CustomCPUTemplatePath: " "}, want: "path requires the custom"},
		{name: "custom path whitespace", cfg: Config{CPUTemplateSelector: CPUTemplateSelector{Kind: CPUTemplateCustom, Digest: "sha256:" + strings.Repeat("a", 64)}, CustomCPUTemplatePath: " /template.json"}, want: "surrounding whitespace"},
		{name: "Firecracker path whitespace", cfg: Config{FirecrackerPath: " firecracker"}, want: "Firecracker path must not contain surrounding whitespace"},
		{name: "Firecracker path only whitespace", cfg: Config{FirecrackerPath: " "}, want: "Firecracker path is required"},
		{name: "helper path whitespace", cfg: Config{CPUTemplateHelperPath: "cpu-template-helper "}, want: "helper path must not contain surrounding whitespace"},
		{name: "helper path only whitespace", cfg: Config{CPUTemplateHelperPath: " "}, want: "helper path is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.WithDefaults().Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestConfigValidateRejectsVCPUCountAboveFirecrackerLimit(t *testing.T) {
	cfg := (Config{VCPUCount: MaxVMVCPUCount + 1}).WithDefaults()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigValidateRejectsStateAndJailerOverlap(t *testing.T) {
	cfg := (Config{
		StateDir:            "/srv/helmr/vms/guest",
		JailerChrootBaseDir: "/srv/helmr/vms/guest/jailer",
	}).WithDefaults()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must be disjoint") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigValidateRejectsRelativeStateAndJailerAlias(t *testing.T) {
	cfg := (Config{
		StateDir:            "worker-state",
		JailerChrootBaseDir: filepath.Join(".", "worker-state", "jailer"),
	}).WithDefaults()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must be disjoint") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigDefaultsClampHealthAttemptToShortHealthTimeout(t *testing.T) {
	cfg := (Config{HealthTimeout: time.Second}).WithDefaults()
	if cfg.HealthAttemptTimeout != time.Second {
		t.Fatalf("HealthAttemptTimeout = %s, want 1s", cfg.HealthAttemptTimeout)
	}
}

func TestConfigValidateRejectsHealthAttemptLongerThanHealthTimeout(t *testing.T) {
	cfg := (Config{
		HealthTimeout:        time.Second,
		HealthAttemptTimeout: 2 * time.Second,
	}).WithDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	if got, want := err.Error(), "guest health attempt timeout"; !strings.Contains(got, want) {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestConfigValidateRejectsNonPositiveInitTimeout(t *testing.T) {
	cfg := (Config{InitTimeout: -time.Second}).WithDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	if got, want := err.Error(), "VMM API initialization timeout"; !strings.Contains(got, want) {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestConfigValidateRequiresBootInputs(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	text := err.Error()
	for _, want := range []string{"Firecracker jailer uid", "Firecracker jailer gid", "guest kernel path", "guest initramfs path", "guest rootfs path", "worker network link pool", "worker network translation pool", "worker network resolver IPv4", "worker network capacity"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q does not contain %q", text, want)
		}
	}
}
