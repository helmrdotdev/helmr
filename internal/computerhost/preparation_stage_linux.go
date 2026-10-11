//go:build linux

package computerhost

import (
	"context"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Assign each resource to the retained owner before another operation can fail.
func (p *PreparedMachines) stagePreparation(ctx context.Context, stage *preparationStage, identity workerapi.AllocationIdentity, start workerapi.PreparationStart) error {
	path := filepath.Join(stage.directory, "image.raw")
	if err := (disk.SeedStore{CAS: p.CAS}).Decode(ctx, disk.SeedArtifact{Object: computerObject(start.SeedObject), LogicalBytes: disk.SeedCapacity}, path, disk.SeedCapacity); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	stage.disk = file
	stage.images, err = p.prepareProgramArtifacts(ctx, stage.directory, identity.InstanceID, &start.Program)
	if err != nil {
		return err
	}
	stage.drives = []vm.ReadOnlyDrive{
		{ID: vm.ProgramRuntimeDrive, Digest: start.Program.Runtime.Digest, SizeBytes: start.Program.Runtime.SizeBytes, MediaType: start.Program.Runtime.MediaType, Source: stage.images.runtime},
		{ID: vm.ProgramDrive, Digest: start.Program.Artifact.Digest, SizeBytes: start.Program.Artifact.SizeBytes, MediaType: start.Program.Artifact.MediaType, Source: stage.images.artifact},
	}
	return nil
}
