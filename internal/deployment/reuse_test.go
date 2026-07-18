package deployment

import (
	"strings"
	"testing"
)

func TestReuseDescriptorCanonicalKey(t *testing.T) {
	descriptor := validReuseDescriptor()
	canonical, err := CanonicalReuseDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := `{"apiVersion":"2026-06-06","architecture":"aarch64","buildRegionId":"us-east-1","bundleFormatVersion":2,"cliVersion":"0.4.0","contentHash":"sha256:` +
		strings.Repeat("1", 64) +
		`","environmentId":"00000000-0000-0000-0000-000000000003","formatVersion":0,"orgId":"00000000-0000-0000-0000-000000000001","projectId":"00000000-0000-0000-0000-000000000002","runtimeDigest":"sha256:` +
		strings.Repeat("2", 64) +
		`","sdkVersion":"0.3.0","workerProtocolVersion":"helmr.worker.v0"}`
	if string(canonical) != wantCanonical {
		t.Fatalf("canonical reuse descriptor = %s, want %s", canonical, wantCanonical)
	}
	key, err := ReuseKey(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if key != "sha256:7c1ff7f7d5056bcde4c059221be3d337be06bd199fd7de00974327baf7bd5a2c" {
		t.Fatalf("reuse key = %q", key)
	}
}

func TestReuseDescriptorRejectsInvalidMembers(t *testing.T) {
	tests := map[string]func(*ReuseDescriptor){
		"format": func(value *ReuseDescriptor) { value.FormatVersion = 1 },
		"org":    func(value *ReuseDescriptor) { value.OrgID = "invalid" },
		"nil org": func(value *ReuseDescriptor) {
			value.OrgID = "00000000-0000-0000-0000-000000000000"
		},
		"project": func(value *ReuseDescriptor) {
			value.ProjectID = "00000000-0000-0000-0000-00000000000A"
		},
		"environment":  func(value *ReuseDescriptor) { value.EnvironmentID = "" },
		"region":       func(value *ReuseDescriptor) { value.BuildRegionID = " us-east-1" },
		"content":      func(value *ReuseDescriptor) { value.ContentHash = "sha256:invalid" },
		"api":          func(value *ReuseDescriptor) { value.APIVersion = "" },
		"sdk":          func(value *ReuseDescriptor) { value.SDKVersion = "v1\n" },
		"cli":          func(value *ReuseDescriptor) { value.CLIVersion = " v1" },
		"invalid utf8": func(value *ReuseDescriptor) { value.CLIVersion = string([]byte{0xff}) },
		"bundle":       func(value *ReuseDescriptor) { value.BundleFormatVersion = 0 },
		"protocol": func(value *ReuseDescriptor) {
			value.WorkerProtocolVersion = "helmr.worker.v1"
		},
		"architecture": func(value *ReuseDescriptor) { value.Architecture = "amd64" },
		"runtime":      func(value *ReuseDescriptor) { value.RuntimeDigest = "sha256:invalid" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			descriptor := validReuseDescriptor()
			mutate(&descriptor)
			if _, err := ReuseKey(descriptor); err == nil {
				t.Fatal("ReuseKey returned nil error")
			}
		})
	}
}

func TestReuseKeyChangesWithEveryRequestMember(t *testing.T) {
	base := validReuseDescriptor()
	baseKey, err := ReuseKey(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*ReuseDescriptor){
		"org":          func(value *ReuseDescriptor) { value.OrgID = "00000000-0000-0000-0000-000000000004" },
		"project":      func(value *ReuseDescriptor) { value.ProjectID = "00000000-0000-0000-0000-000000000004" },
		"environment":  func(value *ReuseDescriptor) { value.EnvironmentID = "00000000-0000-0000-0000-000000000004" },
		"region":       func(value *ReuseDescriptor) { value.BuildRegionID = "eu-west-1" },
		"content":      func(value *ReuseDescriptor) { value.ContentHash = "sha256:" + strings.Repeat("3", 64) },
		"api":          func(value *ReuseDescriptor) { value.APIVersion = "2026-07-01" },
		"sdk":          func(value *ReuseDescriptor) { value.SDKVersion = "0.3.1" },
		"cli":          func(value *ReuseDescriptor) { value.CLIVersion = "0.4.1" },
		"bundle":       func(value *ReuseDescriptor) { value.BundleFormatVersion = 3 },
		"architecture": func(value *ReuseDescriptor) { value.Architecture = ArchitectureX8664 },
		"runtime":      func(value *ReuseDescriptor) { value.RuntimeDigest = "sha256:" + strings.Repeat("4", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			descriptor := base
			mutate(&descriptor)
			key, err := ReuseKey(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if key == baseKey {
				t.Fatalf("reuse key did not change after %s mutation", name)
			}
		})
	}
}

func validReuseDescriptor() ReuseDescriptor {
	return ReuseDescriptor{
		FormatVersion:         ReuseFormatVersion,
		OrgID:                 "00000000-0000-0000-0000-000000000001",
		ProjectID:             "00000000-0000-0000-0000-000000000002",
		EnvironmentID:         "00000000-0000-0000-0000-000000000003",
		BuildRegionID:         "us-east-1",
		ContentHash:           "sha256:" + strings.Repeat("1", 64),
		APIVersion:            "2026-06-06",
		SDKVersion:            "0.3.0",
		CLIVersion:            "0.4.0",
		BundleFormatVersion:   2,
		WorkerProtocolVersion: "helmr.worker.v0",
		Architecture:          ArchitectureAArch64,
		RuntimeDigest:         "sha256:" + strings.Repeat("2", 64),
	}
}
