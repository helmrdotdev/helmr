package computerhost

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact/localcopy"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sync/errgroup"
)

func bootProgram(program *workerapi.RuntimeProgram) (*computercheckpoint.Program, error) {
	if program == nil {
		return nil, nil
	}
	result := &computercheckpoint.Program{Runtime: computercheckpoint.Object(program.Runtime), Artifact: computercheckpoint.Object(program.Artifact)}
	return result, result.Validate()
}
func bootProgramProto(program *computercheckpoint.Program) *computerv0.BootProgram {
	if program == nil {
		return nil
	}
	return &computerv0.BootProgram{Runtime: &computerv0.BootProgramArtifact{Digest: program.Runtime.Digest, SizeBytes: program.Runtime.SizeBytes}, Artifact: &computerv0.BootProgramArtifact{Digest: program.Artifact.Digest, SizeBytes: program.Artifact.SizeBytes}}
}
func bootProgramDrives(program *computercheckpoint.Program, images *programSnapshots) []vm.ReadOnlyDrive {
	if program == nil {
		return nil
	}
	return []vm.ReadOnlyDrive{
		{ID: vm.ProgramRuntimeDrive, Digest: program.Runtime.Digest, SizeBytes: program.Runtime.SizeBytes, MediaType: program.Runtime.MediaType, Source: images.runtime},
		{ID: vm.ProgramDrive, Digest: program.Artifact.Digest, SizeBytes: program.Artifact.SizeBytes, MediaType: program.Artifact.MediaType, Source: images.artifact},
	}
}

// Restore only recreates already admitted immutable backings. The checkpoint
// commits their descriptors; no Session/deployment metadata is re-evaluated.
func (p *PreparedMachines) restoreBootProgram(ctx context.Context, directory string, program *computercheckpoint.Program) (*programSnapshots, error) {
	if program == nil {
		return nil, nil
	}
	if err := program.Validate(); err != nil {
		return nil, invalidCheckpoint(err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	images := &programSnapshots{}
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		started := time.Now()
		images.runtime, err = localcopy.ReadRuntime(ctx, p.ObjectCache.Reader("runtime", p.PlatformStore), directory, p.sessionRuntimeDescriptor(workerapi.CASObject(program.Runtime)))
		slog.Info("Computer boot artifact staged", "role", "runtime", "digest", program.Runtime.Digest, "bytes", program.Runtime.SizeBytes, "duration_ms", float64(time.Since(started))/float64(time.Millisecond), "error_class", vm.RuntimeErrorClass(err))
		return err
	})
	group.Go(func() error {
		var err error
		started := time.Now()
		images.artifact, err = localcopy.ReadProgram(ctx, p.ObjectCache.Reader("program", p.CAS), directory, sessionProgramDescriptor(workerapi.CASObject(program.Artifact)))
		slog.Info("Computer boot artifact staged", "role", "program", "digest", program.Artifact.Digest, "bytes", program.Artifact.SizeBytes, "duration_ms", float64(time.Since(started))/float64(time.Millisecond), "error_class", vm.RuntimeErrorClass(err))
		return err
	})
	if err := group.Wait(); err != nil {
		return nil, errors.Join(err, images.close())
	}
	return images, nil
}
func bootRoleMatches(object workerapi.CASObject, boot computercheckpoint.Object) bool {
	return cas.Descriptor(object) == cas.Descriptor(boot)
}
