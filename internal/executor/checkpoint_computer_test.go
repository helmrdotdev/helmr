package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestCheckpointComputerRequiresExactCapturedDisk(t *testing.T) {
	for _, change := range []string{"none", "missing-capture", "missing-source", "seed", "computer", "digest", "size", "media", "capacity"} {
		t.Run(change, func(t *testing.T) {
			source := retryableWarmTarget().Source
			captured := &workerapi.CheckpointComputer{ComputerID: source.WorkspaceID, LogicalBytes: source.Computer.LogicalBytes, Root: *source.Computer.Root}
			switch change {
			case "missing-capture":
				captured = nil
			case "missing-source":
				source.Computer = nil
			case "seed":
				source.Computer.Seed = &workerapi.ComputerSeed{}
			case "computer":
				captured.ComputerID = "01950000-0000-7000-8000-000000000007"
			case "digest":
				source.Computer.Root.Pack.Digest = sha256sum.DigestBytes([]byte("newer disk with old RAM"))
			case "size":
				captured.Root.Pack.SizeBytes++
			case "media":
				captured.Root.Page.KeyID = "invalid"
			case "capacity":
				captured.LogicalBytes /= 2
			}
			err := validateCheckpointComputerSource(source, captured)
			if (err == nil) != (change == "none") {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}

func TestWarmRuntimeRejectsUnpairedCheckpointBeforeAdmission(t *testing.T) {
	target := retryableWarmTarget()
	target.Source.VMVCPUCount = 2
	target.Source.CPUConfigDigest = sha256sum.DigestBytes([]byte("cpu"))
	manifest := workerapi.CheckpointManifest{RecoveryPoint: workerapi.CheckpointRecoveryPoint{
		ID: "checkpoint-1", RunID: "run-1", AttemptNumber: 1, RunWaitID: "wait-1", CorrelationID: "correlation-1",
		Runtime: workerapi.CheckpointRuntime{Backend: "firecracker", Arch: "x86_64", Contract: "contract", ID: "runtime", KernelDigest: "kernel", InitramfsDigest: "initramfs", RootfsDigest: "root", ConfigDigest: "config", VMVCPUCount: 2, CPUConfigDigest: target.Source.CPUConfigDigest},
	}, RuntimeState: workerapi.CheckpointRuntimeState{ConfigArtifact: workerapi.CheckpointArtifact{Digest: "config", MediaType: "manifest"}}}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	target.Source.Restore = &workerapi.RuntimeRestore{CheckpointID: "checkpoint-1", RunID: "run-1", AttemptNumber: 1, RunWaitID: "wait-1", Manifest: encoded}
	target.PreparationExpiresAt = time.Now().Add(time.Minute)
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	pool.RuntimeArchitecture = deployment.ArchitectureX8664
	admitted := false
	pool.AdmitRuntimeStart = func(context.Context) error { admitted = true; return nil }
	err = pool.warmRuntimeTarget(t.Context(), &typedRuntimeClient{}, target, func() { t.Fatal("unpaired checkpoint started preparation") })
	if err == nil || !strings.Contains(err.Error(), "paired Computer generation") || admitted {
		t.Fatalf("unsafe restore reached admission: admitted=%v err=%v", admitted, err)
	}
}
