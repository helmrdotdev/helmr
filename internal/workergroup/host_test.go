package workergroup

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/jackc/pgx/v5/pgtype"
)

var hostTestGroupID = uuid.MustParse("01900000-0000-7000-8000-000000000401")

// validHostTemplate is the template of a worker host with two CPU shapes,
// 8 vCPUs, 16 GiB and four VM slots.
func validHostTemplate(t *testing.T) Template {
	t.Helper()
	profile := vmplatform.Profile{
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
	}
	id, err := profile.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}
	profile.ID = id
	template := Template{
		Schema: TemplateSchema, Runtime: profile,
		CPUShapes: []vmplatform.CPUShape{
			{VCPUCount: 1, CPUConfigDigest: "sha256:" + strings.Repeat("4", 64)},
			{VCPUCount: 2, CPUConfigDigest: "sha256:" + strings.Repeat("5", 64)},
		},
		Capacity: ResourceVector{CPUMillis: 8_000, MemoryBytes: 16 << 30, GuestEphemeralDiskBytes: 64 << 30, VMSlots: 4},
		PerVM:    ResourceVector{CPUMillis: 2_000, MemoryBytes: 2 << 30, GuestEphemeralDiskBytes: 8 << 30},
	}
	if err := template.Validate(); err != nil {
		t.Fatal(err)
	}
	return template
}

func TestActivationParamsDeriveEpochCapacityFromTemplate(t *testing.T) {
	template := validHostTemplate(t)
	principal := HostPrincipal{HostID: uuid.NewV7(), GroupID: hostTestGroupID, Epoch: 3}
	got := activationParams(principal, Activation{Template: template, CPUEnvironment: []byte(`{}`), CPUEnvironmentDigest: "sha256:" + strings.Repeat("7", 64)})
	want := db.ActivateWorkerHostParams{
		VMPlatformID:   pgtype.Text{String: template.Runtime.ID, Valid: true},
		EpochCPUMillis: 8_000, EpochMemoryBytes: 16 << 30, EpochGuestEphemeralDiskBytes: 64 << 30,
		PerVMCPUMillis: 2_000, PerVMMemoryBytes: 2 << 30, PerVMGuestEphemeralDiskBytes: 8 << 30,
		// Instance starts are bounded by the run slots the host reported.
		MaxVMSlots: 4, MaxVMStarts: 4,
		CPUEnvironment: []byte(`{}`), CPUEnvironmentDigest: pgtype.Text{String: "sha256:" + strings.Repeat("7", 64), Valid: true},
		WorkerHostID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(hostTestGroupID),
		WorkerEpoch: pgtype.Int8{Int64: 3, Valid: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("activation params = %#v, want %#v", got, want)
	}
}

func TestSealPoolParamsDerivePendingPoolContract(t *testing.T) {
	template := validHostTemplate(t)
	poolID := pgvalue.NewUUIDv7()

	got := sealPoolParams(hostTestGroupID, poolID, template)
	want := db.SealWorkerPoolParams{
		VMPlatformID:                    pgtype.Text{String: template.Runtime.ID, Valid: true},
		CapacityCPUMillis:               pgtype.Int8{Int64: template.Capacity.CPUMillis, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: template.Capacity.MemoryBytes, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: template.Capacity.GuestEphemeralDiskBytes, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: template.PerVM.CPUMillis, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: template.PerVM.MemoryBytes, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: template.PerVM.GuestEphemeralDiskBytes, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: int32(template.Capacity.VMSlots), Valid: true},
		WorkerPoolID:                    poolID,
		WorkerGroupID:                   pgvalue.UUID(hostTestGroupID),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seal params = %#v, want %#v", got, want)
	}
}

func TestVMPlatformParamsDeriveCompleteProfile(t *testing.T) {
	profile := validHostTemplate(t).Runtime
	profile.CPUTemplate = vmplatform.CPUTemplateSelector{
		Kind:   vmplatform.CPUTemplateCustom,
		Digest: "sha256:" + strings.Repeat("6", 64),
	}
	var err error
	profile.ID, err = profile.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}

	got := vmPlatformParams(profile)
	want := db.UpsertVMPlatformParams{
		ID:                    profile.ID,
		Arch:                  profile.Arch,
		Contract:              profile.Contract,
		DescriptorDigest:      profile.VMRuntimeDescriptorDigest,
		FirecrackerDigest:     profile.FirecrackerDigest,
		FirecrackerVersion:    profile.FirecrackerVersion,
		SnapshotFormatVersion: profile.SnapshotFormatVersion,
		HostKernelRelease:     profile.HostKernelRelease,
		CPUTemplateKind:       string(profile.CPUTemplate.Kind),
		CPUTemplateDigest:     pgtype.Text{String: profile.CPUTemplate.Digest, Valid: true},
		KernelDigest:          profile.KernelDigest,
		InitramfsDigest:       profile.InitramfsDigest,
		RootfsDigest:          profile.RootfsDigest,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("VM platform params = %#v, want %#v", got, want)
	}
}

func sealedPool(poolID pgtype.UUID, template Template) (db.WorkerPool, []db.WorkerPoolCpuShape) {
	pool := db.WorkerPool{
		ID:                              poolID,
		WorkerGroupID:                   pgvalue.UUID(hostTestGroupID),
		Name:                            "run-primary",
		Status:                          "active",
		VMPlatformID:                    pgtype.Text{String: template.Runtime.ID, Valid: true},
		CapacityCPUMillis:               pgtype.Int8{Int64: template.Capacity.CPUMillis, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: template.Capacity.MemoryBytes, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: template.Capacity.GuestEphemeralDiskBytes, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: template.PerVM.CPUMillis, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: template.PerVM.MemoryBytes, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: template.PerVM.GuestEphemeralDiskBytes, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: int32(template.Capacity.VMSlots), Valid: true},
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

func TestPoolMatchesExactActiveReplay(t *testing.T) {
	template := validHostTemplate(t)
	pool, shapes := sealedPool(pgvalue.NewUUIDv7(), template)
	if !poolMatches(pool, shapes, template) {
		t.Fatal("exact active worker pool replay did not match")
	}
}

func TestPoolMatchesRejectsCPUShapeOrTemplateMismatch(t *testing.T) {
	template := validHostTemplate(t)
	pool, shapes := sealedPool(pgvalue.NewUUIDv7(), template)

	t.Run("CPU shape", func(t *testing.T) {
		changed := append([]db.WorkerPoolCpuShape(nil), shapes...)
		changed[1].CPUConfigDigest = "sha256:" + strings.Repeat("f", 64)
		if poolMatches(pool, changed, template) {
			t.Fatal("worker pool matched a different CPU shape")
		}
	})

	t.Run("CPU template", func(t *testing.T) {
		changed := template
		changed.Runtime.CPUTemplate = vmplatform.CPUTemplateSelector{
			Kind:   vmplatform.CPUTemplateCustom,
			Digest: "sha256:" + strings.Repeat("6", 64),
		}
		var err error
		changed.Runtime.ID, err = changed.Runtime.ExpectedID()
		if err != nil {
			t.Fatal(err)
		}
		if poolMatches(pool, shapes, changed) {
			t.Fatal("worker pool matched a different CPU template")
		}
	})
}

func TestFenceHostRejectsDiagnosticCodesAsControlInputs(t *testing.T) {
	// A call past validation has no database executor; these reasons must stop first.
	for _, reason := range []string{"future_diagnostic", "worker_runtime_invalid", "capture_source_reclaimed", ""} {
		t.Run(reason, func(t *testing.T) {
			var input InputError
			if err := FenceHost(t.Context(), db.New(nil), HostPrincipal{}, reason); !errors.As(err, &input) {
				t.Fatalf("error = %v, want InputError", err)
			}
		})
	}
}

func TestEnrollHostValidatesInputBeforeTheEnrollmentToken(t *testing.T) {
	cfg := testHostAuthConfig(t)
	for _, test := range []struct {
		name       string
		enrollment Enrollment
		input      bool
	}{
		{name: "resource missing", enrollment: Enrollment{PoolName: "default"}, input: true},
		{name: "resource padded", enrollment: Enrollment{PoolName: "default", ResourceID: " i-1"}, input: true},
		{name: "resource too long", enrollment: Enrollment{PoolName: "default", ResourceID: strings.Repeat("r", MaxResourceIDBytes+1)}, input: true},
		{name: "pool name", enrollment: Enrollment{PoolName: "Default", ResourceID: "i-1"}, input: true},
		{name: "token", enrollment: Enrollment{PoolName: "default", ResourceID: "i-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A call past validation has no database executor.
			_, err := EnrollHost(t.Context(), db.New(nil), cfg, test.enrollment)
			var input InputError
			if test.input && !errors.As(err, &input) {
				t.Fatalf("error = %v, want InputError", err)
			}
			if !test.input && !errors.Is(err, ErrInvalidEnrollmentToken) {
				t.Fatalf("error = %v, want ErrInvalidEnrollmentToken", err)
			}
		})
	}
}
