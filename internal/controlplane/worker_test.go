package controlplane

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerActivationDerivesRuntimeStartsFromRunSlots(t *testing.T) {
	worker := workerActor{WorkerGroupID: "group", WorkerEpoch: 1}
	runWorker := validWorkerCapabilities(t)
	if got := workerActivationParams(worker, runWorker, []byte(`{}`)).MaxRuntimeStarts; got != runWorker.ExecutionSlotsAvailable {
		t.Fatalf("run max runtime starts = %d, want %d", got, runWorker.ExecutionSlotsAvailable)
	}
	buildWorker := validBuildOnlyWorkerCapabilities(t)
	if got := workerActivationParams(worker, buildWorker, []byte(`{}`)).MaxRuntimeStarts; got != 0 {
		t.Fatalf("build max runtime starts = %d, want zero", got)
	}
}

func validWorkerCapabilities(t *testing.T) workerapi.Capabilities {
	t.Helper()
	c := workerapi.Capabilities{
		Runtime: capacityapi.RuntimeProfile{
			Arch: "x86_64", Contract: capacityapi.RuntimeContract,
			VMRuntimeDescriptorDigest: "sha256:" + strings.Repeat("a", 64),
			FirecrackerDigest:         "sha256:" + strings.Repeat("b", 64),
			FirecrackerVersion:        "1.16.1",
			SnapshotFormatVersion:     "6.0.0",
			HostKernelRelease:         "6.8.0-1024-aws",
			CPUTemplate:               capacityapi.CPUTemplateSelector{Kind: capacityapi.CPUTemplateNone},
			KernelDigest:              "sha256:" + strings.Repeat("1", 64),
			InitramfsDigest:           "sha256:" + strings.Repeat("2", 64),
			RootfsDigest:              "sha256:" + strings.Repeat("3", 64),
		},
		CPUShapes: []capacityapi.CPUShape{
			{VCPUCount: 1, CPUConfigDigest: "sha256:" + strings.Repeat("4", 64)},
			{VCPUCount: 2, CPUConfigDigest: "sha256:" + strings.Repeat("5", 64)},
		},
		CPUEnvironment: workerapi.CPUEnvironment{
			FirecrackerVersion: "1.16.1", HostKernelRelease: "6.8.0-1024-aws",
			MicrocodeVersion: "0x2b000643", BIOSVersion: "1.0", BIOSRevision: "1.0",
		},
		SubstrateFormat:           capacityapi.SubstrateFormatExt4,
		SubstrateContract:         capacityapi.SubstrateContractExt4,
		MaxVCPUs:                  8,
		MaxMemoryMiB:              16 << 10,
		VMMilliCPU:                2_000,
		VMMemoryMiB:               2 << 10,
		GuestEphemeralDiskBytes:   64 << 30,
		VMGuestEphemeralDiskBytes: 8 << 30,
		ExecutionSlotsAvailable:   4,
		SupportsRun:               true,
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

func validBuildOnlyWorkerCapabilities(t *testing.T) workerapi.Capabilities {
	t.Helper()
	c := validWorkerCapabilities(t)
	c.SubstrateFormat = ""
	c.SubstrateContract = ""
	c.ExecutionSlotsAvailable = 0
	c.SupportsRun = false
	c.SupportsBuild = true
	c.MaxBuildExecutors = 1
	return c
}

func TestNormalizeWorkerCapabilitiesReturnsCanonicalCompleteEvidence(t *testing.T) {
	want := validWorkerCapabilities(t)
	want.Runtime.CPUTemplate = capacityapi.CPUTemplateSelector{
		Kind:   capacityapi.CPUTemplateCustom,
		Digest: "sha256:" + strings.Repeat("6", 64),
	}
	var err error
	want.Runtime.ID, err = want.Runtime.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}

	input := want
	input.CPUShapes = append([]capacityapi.CPUShape(nil), want.CPUShapes...)
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
	input.SubstrateFormat = " " + input.SubstrateFormat + " "
	input.SubstrateContract = " " + input.SubstrateContract + " "

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
	capabilities.SupportsBuild = true
	capabilities.MaxBuildExecutors = 1

	want := capacityapi.WorkerTemplate{
		Schema:        capacityapi.WorkerTemplateSchema,
		SupportsRun:   true,
		SupportsBuild: true,
		Runtime:       capabilities.Runtime,
		CPUShapes:     append([]capacityapi.CPUShape(nil), capabilities.CPUShapes...),
		Substrate: capacityapi.SubstrateProfile{
			Format: capacityapi.SubstrateFormatExt4, Contract: capacityapi.SubstrateContractExt4,
		},
		Capacity: capacityapi.ResourceVector{
			CPUMillis: 8_000, MemoryBytes: 16 << 30, GuestEphemeralDiskBytes: 64 << 30,
			VMSlots: 4, BuildExecutors: 1,
		},
		PerVM: capacityapi.ResourceVector{
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

func TestSealWorkerPoolParamsDerivePendingPoolContract(t *testing.T) {
	capabilities := validWorkerCapabilities(t)
	capabilities.SupportsBuild = true
	capabilities.MaxBuildExecutors = 1
	template := workerTemplate(capabilities)
	poolID := pgvalue.NewUUIDv7()

	got := sealWorkerPoolParams("workers", poolID, template)
	want := db.SealWorkerPoolParams{
		RuntimeIdentityID:               pgtype.Text{String: template.Runtime.ID, Valid: true},
		SubstrateFormat:                 pgtype.Text{String: template.Substrate.Format, Valid: true},
		SubstrateContract:               pgtype.Text{String: template.Substrate.Contract, Valid: true},
		CapacityCPUMillis:               pgtype.Int8{Int64: template.Capacity.CPUMillis, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: template.Capacity.MemoryBytes, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: template.Capacity.GuestEphemeralDiskBytes, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: template.PerVM.CPUMillis, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: template.PerVM.MemoryBytes, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: template.PerVM.GuestEphemeralDiskBytes, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: int32(template.Capacity.VMSlots), Valid: true},
		MaxBuildExecutors:               pgtype.Int4{Int32: int32(template.Capacity.BuildExecutors), Valid: true},
		WorkerPoolID:                    poolID,
		WorkerGroupID:                   "workers",
		AllowsRun:                       true,
		AllowsBuild:                     true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seal params = %#v, want %#v", got, want)
	}
}

func TestRuntimeIdentityParamsDeriveCompleteProfile(t *testing.T) {
	profile := validWorkerCapabilities(t).Runtime
	profile.CPUTemplate = capacityapi.CPUTemplateSelector{
		Kind:   capacityapi.CPUTemplateCustom,
		Digest: "sha256:" + strings.Repeat("6", 64),
	}
	var err error
	profile.ID, err = profile.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}

	got := runtimeIdentityParams(profile)
	want := db.UpsertRuntimeIdentityParams{
		ID:                        profile.ID,
		RuntimeArch:               profile.Arch,
		VMRuntimeContract:         profile.Contract,
		VMRuntimeDescriptorDigest: profile.VMRuntimeDescriptorDigest,
		FirecrackerDigest:         profile.FirecrackerDigest,
		FirecrackerVersion:        profile.FirecrackerVersion,
		SnapshotFormatVersion:     profile.SnapshotFormatVersion,
		HostKernelRelease:         profile.HostKernelRelease,
		CPUTemplateKind:           string(profile.CPUTemplate.Kind),
		CPUTemplateDigest:         pgtype.Text{String: profile.CPUTemplate.Digest, Valid: true},
		KernelDigest:              profile.KernelDigest,
		InitramfsDigest:           profile.InitramfsDigest,
		RootfsDigest:              profile.RootfsDigest,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime identity params = %#v, want %#v", got, want)
	}
}

func TestWorkerPoolMatchesExactActiveReplay(t *testing.T) {
	template := workerTemplate(validWorkerCapabilities(t))
	poolID := pgvalue.NewUUIDv7()
	pool, shapes := sealedWorkerPool(poolID, "workers", "run-primary", template)

	if !workerPoolMatches(pool, shapes, template) {
		t.Fatal("exact active Worker Pool replay did not match")
	}
}

func TestWorkerPoolMatchesRejectsCPUShapeOrTemplateMismatch(t *testing.T) {
	template := workerTemplate(validWorkerCapabilities(t))
	poolID := pgvalue.NewUUIDv7()
	pool, shapes := sealedWorkerPool(poolID, "workers", "run-primary", template)

	t.Run("CPU shape", func(t *testing.T) {
		changed := append([]db.WorkerPoolCpuShape(nil), shapes...)
		changed[1].CPUConfigDigest = "sha256:" + strings.Repeat("f", 64)
		if workerPoolMatches(pool, changed, template) {
			t.Fatal("Worker Pool matched a different CPU shape")
		}
	})

	t.Run("CPU template", func(t *testing.T) {
		changed := template
		changed.Runtime.CPUTemplate = capacityapi.CPUTemplateSelector{
			Kind:   capacityapi.CPUTemplateCustom,
			Digest: "sha256:" + strings.Repeat("6", 64),
		}
		var err error
		changed.Runtime.ID, err = changed.Runtime.ExpectedID()
		if err != nil {
			t.Fatal(err)
		}
		if workerPoolMatches(pool, shapes, changed) {
			t.Fatal("Worker Pool matched a different CPU template")
		}
	})
}

func TestNormalizeWorkerCapabilitiesEnforcesRunBuildRoles(t *testing.T) {
	dual := validWorkerCapabilities(t)
	dual.SupportsBuild = true
	dual.MaxBuildExecutors = 1

	for _, test := range []struct {
		name         string
		capabilities workerapi.Capabilities
	}{
		{name: "run", capabilities: validWorkerCapabilities(t)},
		{name: "build", capabilities: validBuildOnlyWorkerCapabilities(t)},
		{name: "run and build", capabilities: dual},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeWorkerCapabilities(test.capabilities)
			if err != nil {
				t.Fatal(err)
			}
			if got.SupportsRun != test.capabilities.SupportsRun || got.SupportsBuild != test.capabilities.SupportsBuild {
				t.Fatalf("roles = run:%t build:%t", got.SupportsRun, got.SupportsBuild)
			}
			if err := workerTemplate(got).Validate(); err != nil {
				t.Fatalf("normalized role template is invalid: %v", err)
			}
		})
	}

	for _, test := range []struct {
		name   string
		mutate func(*workerapi.Capabilities)
	}{
		{name: "role required", mutate: func(c *workerapi.Capabilities) {
			c.SupportsRun = false
		}},
		{name: "run substrate required", mutate: func(c *workerapi.Capabilities) {
			c.SubstrateFormat = ""
			c.SubstrateContract = ""
		}},
		{name: "run slots required", mutate: func(c *workerapi.Capabilities) {
			c.ExecutionSlotsAvailable = 0
		}},
		{name: "build only has no run substrate", mutate: func(c *workerapi.Capabilities) {
			*c = validBuildOnlyWorkerCapabilities(t)
			c.SubstrateFormat = capacityapi.SubstrateFormatExt4
			c.SubstrateContract = capacityapi.SubstrateContractExt4
		}},
		{name: "build only has no run slots", mutate: func(c *workerapi.Capabilities) {
			*c = validBuildOnlyWorkerCapabilities(t)
			c.ExecutionSlotsAvailable = 1
		}},
		{name: "build executor required", mutate: func(c *workerapi.Capabilities) {
			c.SupportsBuild = true
		}},
		{name: "run only has no build executor", mutate: func(c *workerapi.Capabilities) {
			c.MaxBuildExecutors = 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			capabilities := validWorkerCapabilities(t)
			test.mutate(&capabilities)
			if _, err := normalizeWorkerCapabilities(capabilities); err == nil {
				t.Fatal("invalid Worker role contract was accepted")
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
		capabilities.Runtime.CPUTemplate = capacityapi.CPUTemplateSelector{
			Kind:   capacityapi.CPUTemplateCustom,
			Digest: "sha256:" + strings.Repeat("6", 64),
		}
		if _, err := normalizeWorkerCapabilities(capabilities); err == nil || !strings.Contains(err.Error(), "runtime.id") {
			t.Fatalf("error = %v, want RuntimeProfile identity error", err)
		}
	})
}

func sealedWorkerPool(
	poolID pgtype.UUID,
	groupID string,
	name string,
	template capacityapi.WorkerTemplate,
) (db.WorkerPool, []db.WorkerPoolCpuShape) {
	pool := db.WorkerPool{
		ID:                              poolID,
		WorkerGroupID:                   groupID,
		Name:                            name,
		State:                           "active",
		AllowsRun:                       template.SupportsRun,
		AllowsBuild:                     template.SupportsBuild,
		RuntimeIdentityID:               pgtype.Text{String: template.Runtime.ID, Valid: true},
		SubstrateFormat:                 pgtype.Text{String: template.Substrate.Format, Valid: true},
		SubstrateContract:               pgtype.Text{String: template.Substrate.Contract, Valid: true},
		CapacityCPUMillis:               pgtype.Int8{Int64: template.Capacity.CPUMillis, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: template.Capacity.MemoryBytes, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: template.Capacity.GuestEphemeralDiskBytes, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: template.PerVM.CPUMillis, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: template.PerVM.MemoryBytes, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: template.PerVM.GuestEphemeralDiskBytes, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: int32(template.Capacity.VMSlots), Valid: true},
		MaxBuildExecutors:               pgtype.Int4{Int32: int32(template.Capacity.BuildExecutors), Valid: true},
		SealedAt:                        pgtype.Timestamptz{Time: time.Unix(1, 0).UTC(), Valid: true},
	}
	shapes := make([]db.WorkerPoolCpuShape, len(template.CPUShapes))
	for index, shape := range template.CPUShapes {
		shapes[index] = db.WorkerPoolCpuShape{
			WorkerPoolID: poolID, VCPUCount: shape.VCPUCount, CPUConfigDigest: shape.CPUConfigDigest,
		}
	}
	return pool, shapes
}

func TestWorkerRoleReadinessReportsMissingObservation(t *testing.T) {
	readiness := workerRoleReadiness(db.GetWorkerInstanceStateRow{
		State: db.WorkerInstanceStateActive,
	}, false, pgtype.Text{})
	if readiness.Ready || readiness.PausedReason != "observation_missing" {
		t.Fatalf("readiness = %+v, want observation_missing", readiness)
	}
}

func TestValidateWorkerStartupRecoveryRequiresCanonicalUUIDv7(t *testing.T) {
	now := time.Now().UTC()
	valid := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	base := workerapi.StartupRecoveryRequest{
		InventoryComplete: true,
		InventoryScope:    "worker_runtime_state_roots_v0",
		ObservedAt:        now,
		Inventory:         []string{valid},
		Reclaimed:         []string{valid},
	}
	for _, test := range []struct {
		name string
		id   string
	}{
		{name: "uuidv4", id: "8fa3431e-c649-4ea0-bf12-b8e9fcdf1d8d"},
		{name: "uppercase", id: "019C10D5-A6F7-7AF1-8F5F-BB97BCC0DC31"},
		{name: "whitespace", id: " " + valid},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Inventory = []string{test.id}
			request.Reclaimed = []string{test.id}
			err := validateWorkerStartupRecovery(request, now.Add(-time.Minute), now)
			if err == nil || !strings.Contains(err.Error(), "canonical UUIDv7") {
				t.Fatalf("error = %v, want canonical UUIDv7 rejection", err)
			}
		})
	}
}
