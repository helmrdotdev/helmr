package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Preparation belongs to the Instance; a Computer may be prepared before any
// Program is selected. Checkpoint restoration attaches its captured state separately.
func projectComputerInstancePreparation(ctx context.Context, store cas.Reader, row db.ListComputerInstanceReconcileTargetsRow) (workerapi.RuntimeSource, error) {
	disk, err := projectRuntimeComputerSource(row)
	if err != nil {
		return workerapi.RuntimeSource{
			WriterGeneration: row.WriterGeneration}, err
	}
	source := computerInstanceSourceMetadata(row)
	source.Computer = &disk
	if !row.ProgramDeploymentID.Valid {
		return source, nil
	}
	if !row.ProgramRuntimeDigest.Valid || !row.ProgramArtifactDigest.Valid || !row.ProgramArtifactSizeBytes.Valid || !row.ProgramArtifactMediaType.Valid {
		return workerapi.RuntimeSource{
			WriterGeneration: row.WriterGeneration}, errors.New("computer instance Program authority is incomplete")
	}
	program, err := projectRuntimeProgram(ctx, runtimeProgramAuthorityFromDeployment(row.ProgramDeploymentID, row.ProgramRuntimeDigest.String, row.ProgramArtifactDigest.String, row.ProgramArtifactSizeBytes.Int64, row.ProgramArtifactMediaType.String, row.ProgramIndexDigest), row.ComputerArchitecture, store)
	if err != nil {
		return workerapi.RuntimeSource{
			WriterGeneration: row.WriterGeneration}, fmt.Errorf("project computer instance Program: %w", err)
	}
	source.Program = &program
	return source, nil
}

func computerInstanceSourceMetadata(row db.ListComputerInstanceReconcileTargetsRow) workerapi.RuntimeSource {
	return workerapi.RuntimeSource{
		WriterGeneration: row.WriterGeneration,
		ComputerSpecID:   pgvalue.UUIDString(row.ComputerSpecID), ComputerID: pgvalue.UUIDString(row.ComputerID),
		VMPlatformID: row.VMPlatformID, VMVCPUCount: row.VMVCPUCount, CPUConfigDigest: row.CPUConfigDigest,
		ComputerImage:        workerapi.CASObject{Digest: row.ComputerImageDigest, SizeBytes: row.ComputerImageSizeBytes, MediaType: row.ComputerImageMediaType},
		ComputerArchitecture: row.ComputerArchitecture, RootfsDigest: row.RootfsDigest,
		ReservedCPUMillis: int32(row.ReservedCPUMillis), ReservedMemoryMiB: int32(row.ReservedMemoryBytes / 1048576),
		ReservedDiskMiB: row.ReservedGuestEphemeralDiskBytes / 1048576, ReservedExecutionSlots: row.ReservedExecutionSlots,
		VMRuntimeContract: row.VmContract,
	}
}
