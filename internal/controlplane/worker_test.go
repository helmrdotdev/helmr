package controlplane

import (
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerActivationDerivesRuntimeStartsFromRunSlots(t *testing.T) {
	worker := workerActor{WorkerGroupID: "group", WorkerEpoch: 1}
	runWorker := validWorkerCapabilities(t)
	if got := workerActivationParams(worker, runWorker).MaxRuntimeStarts; got != runWorker.ExecutionSlotsAvailable {
		t.Fatalf("run max runtime starts = %d, want %d", got, runWorker.ExecutionSlotsAvailable)
	}
	buildWorker := runWorker
	buildWorker.SupportsRun = false
	if got := workerActivationParams(worker, buildWorker).MaxRuntimeStarts; got != 0 {
		t.Fatalf("build max runtime starts = %d, want zero", got)
	}
}

func validWorkerCapabilities(t *testing.T) workerapi.Capabilities {
	t.Helper()
	c := workerapi.Capabilities{
		WorkerVersion:             "dev",
		RuntimeArch:               "x86_64",
		VMRuntimeContract:         runtimeid.Contract,
		KernelDigest:              "sha256:kernel",
		InitramfsDigest:           "sha256:initramfs",
		RootfsDigest:              "sha256:rootfs",
		SubstrateFormat:           "ext4",
		SubstrateContract:         "helmr.substrate.v0",
		MaxVCPUs:                  8,
		MaxMemoryMiB:              16 << 10,
		VMMilliCPU:                2_000,
		VMMemoryMiB:               2 << 10,
		GuestEphemeralDiskBytes:   64 << 30,
		VMGuestEphemeralDiskBytes: 8 << 30,
		ExecutionSlotsAvailable:   4,
		SupportsRun:               true,
	}
	id, err := runtimeid.Digest(runtimeid.Selector{
		Arch: c.RuntimeArch, Contract: c.VMRuntimeContract,
		KernelDigest: c.KernelDigest, InitramfsDigest: c.InitramfsDigest, RootfsDigest: c.RootfsDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.RuntimeID = id
	return c
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
