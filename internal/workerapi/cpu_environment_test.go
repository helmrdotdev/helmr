package workerapi

import "testing"

func TestCPUEnvironmentDigestAndValidation(t *testing.T) {
	environment := CPUEnvironment{
		FirecrackerVersion: "1.16.1",
		HostKernelRelease:  "6.8.0-1024-aws",
		MicrocodeVersion:   "0x2b000643",
		BIOSVersion:        "1.0",
		BIOSRevision:       "2.0",
	}
	var err error
	environment.Digest, err = environment.ExpectedDigest()
	if err != nil {
		t.Fatal(err)
	}
	// Fixed SHA-256 vector of the canonical, ordered JSON payload.
	const wantDigest = "sha256:0e5dca38ec6734633a6f6d1c4ee00d35616b6b3a3830c34b9dc7a215068b6caf"
	if environment.Digest != wantDigest {
		t.Fatalf("CPU environment digest = %q, want %q", environment.Digest, wantDigest)
	}
	if err := environment.Validate(); err != nil {
		t.Fatalf("validate canonical CPU environment: %v", err)
	}

	for _, field := range []struct {
		name   string
		change func(*CPUEnvironment)
	}{
		{"firecracker version", func(e *CPUEnvironment) { e.FirecrackerVersion = "1.16.2" }},
		{"host kernel release", func(e *CPUEnvironment) { e.HostKernelRelease = "6.8.0-1025-aws" }},
		{"microcode version", func(e *CPUEnvironment) { e.MicrocodeVersion = "0x2b000644" }},
		{"BIOS version", func(e *CPUEnvironment) { e.BIOSVersion = "1.1" }},
		{"BIOS revision", func(e *CPUEnvironment) { e.BIOSRevision = "2.1" }},
	} {
		t.Run(field.name, func(t *testing.T) {
			changed := environment
			field.change(&changed)
			if err := changed.Validate(); err == nil {
				t.Fatal("CPU environment with stale digest was accepted")
			}
			var err error
			changed.Digest, err = changed.ExpectedDigest()
			if err != nil {
				t.Fatal(err)
			}
			if err := changed.Validate(); err != nil {
				t.Fatalf("validate changed CPU environment: %v", err)
			}
		})
	}
}

func TestCPUEnvironmentRejectsNoncanonicalField(t *testing.T) {
	environment := CPUEnvironment{
		FirecrackerVersion: "1.16.1",
		HostKernelRelease:  "6.8.0-1024-aws",
		MicrocodeVersion:   " 0x2b000643",
		BIOSVersion:        "1.0",
		BIOSRevision:       "1.0",
	}
	if _, err := environment.ExpectedDigest(); err == nil {
		t.Fatal("noncanonical CPU environment was accepted")
	}
}
