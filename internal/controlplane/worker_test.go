package controlplane

import (
	"reflect"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func validWorkerCapabilities(t *testing.T) workerapi.Capabilities {
	t.Helper()
	c := workerapi.Capabilities{
		Runtime: vmplatform.Profile{
			Arch: "x86_64", Contract: vmplatform.Contract,
			VMRuntimeDescriptorDigest: "sha256:" + strings.Repeat("a", 64),
			FirecrackerDigest:         "sha256:" + strings.Repeat("b", 64),
			FirecrackerVersion:        "1.16.1",
			SnapshotFormatVersion:     "6.0.0",
			HostKernelRelease:         "6.8.0-1024-aws",
			CPUTemplate:               vmplatform.CPUTemplateSelector{Kind: vmplatform.CPUTemplateNone},
			KernelDigest:              "sha256:" + strings.Repeat("1", 64),
			InitramfsDigest:           "sha256:" + strings.Repeat("2", 64),
			RootfsDigest:              "sha256:" + strings.Repeat("3", 64),
		},
		CPUShapes: []vmplatform.CPUShape{
			{VCPUCount: 1, CPUConfigDigest: "sha256:" + strings.Repeat("4", 64)},
			{VCPUCount: 2, CPUConfigDigest: "sha256:" + strings.Repeat("5", 64)},
		},
		CPUEnvironment: workerapi.CPUEnvironment{
			FirecrackerVersion: "1.16.1", HostKernelRelease: "6.8.0-1024-aws",
			MicrocodeVersion: "0x2b000643", BIOSVersion: "1.0", BIOSRevision: "1.0",
		},
		MaxVCPUs:                  8,
		MaxMemoryMiB:              16 << 10,
		VMMilliCPU:                2_000,
		VMMemoryMiB:               2 << 10,
		GuestEphemeralDiskBytes:   64 << 30,
		VMGuestEphemeralDiskBytes: 8 << 30,
		ExecutionSlotsAvailable:   4,
	}
	id, err := c.Runtime.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}
	c.Runtime.ID = id
	c.CPUEnvironment.Digest, err = c.CPUEnvironment.ExpectedDigest()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNormalizeWorkerCapabilitiesReturnsCanonicalCompleteEvidence(t *testing.T) {
	want := validWorkerCapabilities(t)
	want.Runtime.CPUTemplate = vmplatform.CPUTemplateSelector{
		Kind:   vmplatform.CPUTemplateCustom,
		Digest: "sha256:" + strings.Repeat("6", 64),
	}
	var err error
	want.Runtime.ID, err = want.Runtime.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}

	input := want
	input.CPUShapes = append([]vmplatform.CPUShape(nil), want.CPUShapes...)
	input.Runtime.ID = " \t" + input.Runtime.ID + "\n"
	input.Runtime.Arch = " " + input.Runtime.Arch + " "
	input.Runtime.Contract = "\t" + input.Runtime.Contract + "\n"
	input.Runtime.VMRuntimeDescriptorDigest = " " + input.Runtime.VMRuntimeDescriptorDigest + " "
	input.Runtime.FirecrackerDigest = " " + input.Runtime.FirecrackerDigest + " "
	input.Runtime.FirecrackerVersion = " " + input.Runtime.FirecrackerVersion + " "
	input.Runtime.SnapshotFormatVersion = " " + input.Runtime.SnapshotFormatVersion + " "
	input.Runtime.HostKernelRelease = " " + input.Runtime.HostKernelRelease + " "
	input.Runtime.CPUTemplate.Digest = " " + input.Runtime.CPUTemplate.Digest + " "
	input.Runtime.KernelDigest = " " + input.Runtime.KernelDigest + " "
	input.Runtime.InitramfsDigest = " " + input.Runtime.InitramfsDigest + " "
	input.Runtime.RootfsDigest = " " + input.Runtime.RootfsDigest + " "
	for index := range input.CPUShapes {
		input.CPUShapes[index].CPUConfigDigest = " " + input.CPUShapes[index].CPUConfigDigest + " "
	}
	input.CPUEnvironment.Digest = " " + input.CPUEnvironment.Digest + " "
	input.CPUEnvironment.FirecrackerVersion = " " + input.CPUEnvironment.FirecrackerVersion + " "
	input.CPUEnvironment.HostKernelRelease = " " + input.CPUEnvironment.HostKernelRelease + " "
	input.CPUEnvironment.MicrocodeVersion = " " + input.CPUEnvironment.MicrocodeVersion + " "
	input.CPUEnvironment.BIOSVersion = " " + input.CPUEnvironment.BIOSVersion + " "
	input.CPUEnvironment.BIOSRevision = " " + input.CPUEnvironment.BIOSRevision + " "

	got, err := normalizeWorkerCapabilities(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized capabilities = %#v, want %#v", got, want)
	}
	if got.Runtime.ID == "" || got.CPUEnvironment.Digest == "" || len(got.CPUShapes) != 2 {
		t.Fatalf("normalized capabilities omit runtime evidence: %#v", got)
	}
}

func TestWorkerTemplateDerivesImmutablePoolContract(t *testing.T) {
	capabilities := validWorkerCapabilities(t)

	want := workergroup.Template{
		Schema:    workergroup.TemplateSchema,
		Runtime:   capabilities.Runtime,
		CPUShapes: append([]vmplatform.CPUShape(nil), capabilities.CPUShapes...),
		Capacity: workergroup.ResourceVector{
			CPUMillis: 8_000, MemoryBytes: 16 << 30, GuestEphemeralDiskBytes: 64 << 30,
			VMSlots: 4,
		},
		PerVM: workergroup.ResourceVector{
			CPUMillis: 2_000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 8 << 30,
		},
	}

	got := workerTemplate(capabilities)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Worker template = %#v, want %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("derived Worker template is invalid: %v", err)
	}
	got.CPUShapes[0].CPUConfigDigest = "sha256:" + strings.Repeat("f", 64)
	if capabilities.CPUShapes[0].CPUConfigDigest != want.CPUShapes[0].CPUConfigDigest {
		t.Fatal("Worker template aliases the activation CPU shape slice")
	}
}

func TestNormalizeWorkerCapabilitiesEnforcesExecutionContract(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*workerapi.Capabilities)
	}{
		{name: "execution slots required", mutate: func(c *workerapi.Capabilities) {
			c.ExecutionSlotsAvailable = 0
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			capabilities := validWorkerCapabilities(t)
			test.mutate(&capabilities)
			if _, err := normalizeWorkerCapabilities(capabilities); err == nil {
				t.Fatal("invalid execution Worker contract was accepted")
			}
		})
	}
}

func TestNormalizeWorkerCapabilitiesRejectsCPUShapeOrTemplateMismatch(t *testing.T) {
	t.Run("CPU shape coverage", func(t *testing.T) {
		capabilities := validWorkerCapabilities(t)
		capabilities.CPUShapes = capabilities.CPUShapes[:1]
		if _, err := normalizeWorkerCapabilities(capabilities); err == nil || !strings.Contains(err.Error(), "cpu_shapes") {
			t.Fatalf("error = %v, want CPU shape coverage error", err)
		}
	})

	t.Run("CPU template identity", func(t *testing.T) {
		capabilities := validWorkerCapabilities(t)
		capabilities.Runtime.CPUTemplate = vmplatform.CPUTemplateSelector{
			Kind:   vmplatform.CPUTemplateCustom,
			Digest: "sha256:" + strings.Repeat("6", 64),
		}
		if _, err := normalizeWorkerCapabilities(capabilities); err == nil || !strings.Contains(err.Error(), "runtime.id") {
			t.Fatalf("error = %v, want RuntimeProfile identity error", err)
		}
	})
}

func TestWorkerRoleReadinessReportsMissingObservation(t *testing.T) {
	readiness := workerRoleReadiness(db.GetWorkerHostStatusRow{
		Status: db.WorkerHostStatusActive,
	}, false, pgtype.Text{})
	if readiness.Ready || readiness.PausedReason != "observation_missing" {
		t.Fatalf("readiness = %+v, want observation_missing", readiness)
	}
}

func TestValidateWorkerStartupRecoveryRequiresCanonicalUUIDv7(t *testing.T) {
	valid := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	for _, ids := range [][]string{{"8fa3431e-c649-4ea0-bf12-b8e9fcdf1d8d"}, {"019C10D5-A6F7-7AF1-8F5F-BB97BCC0DC31"}, {" " + valid}} {
		if err := validateWorkerStartupRecovery(workerapi.StartupRecoveryRequest{Quarantined: ids}); err == nil || !strings.Contains(err.Error(), "canonical UUIDv7") {
			t.Fatalf("invalid IDs=%v: %v", ids, err)
		}
	}
	for _, ids := range [][]string{nil, {valid, valid}} {
		if err := validateWorkerStartupRecovery(workerapi.StartupRecoveryRequest{Quarantined: ids}); err == nil {
			t.Fatalf("accepted invalid array %v", ids)
		}
	}
	for _, ids := range [][]string{{}, {valid}} {
		if err := validateWorkerStartupRecovery(workerapi.StartupRecoveryRequest{Quarantined: ids}); err != nil {
			t.Fatalf("rejected valid array %v: %v", ids, err)
		}
	}
}
