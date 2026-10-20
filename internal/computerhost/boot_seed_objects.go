package computerhost

import (
	"context"
	"errors"
	"os"
	"uuid"

	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sync/errgroup"
)

// publishBootSeed retains encrypted bytes only. The build owner must qualify
// independent clones before registering this manifest as eligible. Its staging
// directory remains charged and owned by the caller until this method returns.
func (p *PreparedMachines) publishBootSeed(ctx context.Context, directory string, environmentID, buildID uuid.UUID, artifact vm.BootSeedArtifact, scratchBytes int64) (bootseed.Manifest, error) {
	m := bootseed.Manifest{EnvironmentID: environmentID, BuildID: buildID,
		Config: append([]byte(nil), artifact.Manifest...), MemoryBytes: artifact.MemoryBytes, ScratchBytes: scratchBytes}
	if p.ComputerObjects == nil || p.CheckpointCipher == nil || environmentID == uuid.Nil() || buildID == uuid.Nil() {
		return m, errors.New("boot seed publication requires build ownership and encrypted storage")
	}
	scratchLimit, err := filepack.PackedSizeLimit(scratchBytes, filepack.ScratchRole)
	if err != nil {
		return m, err
	}
	type staged struct {
		role, media string
		file        vm.SnapshotFile
		limit       int64
		object      *cas.Descriptor
		path        string
	}
	files := []staged{
		{role: "vm_state", media: cas.BootSeedVMStateMediaType, file: artifact.VMState, limit: vm.SnapshotStateLimit, object: &m.VMState},
		{role: "memory", media: cas.BootSeedMemoryMediaType, file: artifact.Memory, limit: artifact.MemoryBytes, object: &m.Memory},
		{role: "scratch_disk", media: cas.BootSeedScratchMediaType, file: artifact.ScratchDisk, limit: scratchLimit, object: &m.ScratchDisk},
	}
	for i := range files {
		item := &files[i]
		if item.file.MediaType != item.media {
			return m, errors.New("boot seed artifact has the wrong media type")
		}
		f, err := os.Open(item.file.Path)
		if err != nil {
			return m, err
		}
		var object computercheckpoint.Object
		if item.role == "memory" {
			object, item.path, m.MemoryHeaderDigest, err = encryptCheckpointMemory(ctx, p.CheckpointCipher, directory, m.Purpose()+"/memory", f, artifact.MemoryBytes)
			object.MediaType = item.media
		} else {
			object, item.path, err = encryptCheckpointObject(ctx, p.CheckpointCipher, directory, m.Purpose(), item.role, item.media, f, item.limit)
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return m, err
		}
		*item.object = cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
	}
	if err := m.Validate(); err != nil {
		return m, err
	}
	group, uploadCtx := errgroup.WithContext(ctx)
	group.SetLimit(2)
	for _, item := range files {
		group.Go(func() error {
			f, err := os.Open(item.path)
			if err != nil {
				return err
			}
			object, err := p.ComputerObjects.Publish(uploadCtx, *item.object, f)
			if err == nil {
				err = cas.RequireExact(object, *item.object)
			}
			return errors.Join(err, f.Close())
		})
	}
	return m, group.Wait()
}

// openBootSeed stages bounded state/scratch and authenticates the memory index.
// RAM remains demand-readable. The caller retains the source until its VM and
// pager have stopped, then releases the allocation's staging directory.
func (p *PreparedMachines) openBootSeed(ctx context.Context, directory string, m bootseed.Manifest, memoryBytes, scratchBytes int64) (*vm.BootSeedRestore, error) {
	if err := m.Validate(); err != nil {
		return nil, invalidCheckpoint(err)
	}
	if m.MemoryBytes != memoryBytes || m.ScratchBytes != scratchBytes ||
		p.ComputerObjects == nil || p.ComputerRanges == nil || p.CheckpointCipher == nil {
		return nil, errors.New("boot seed differs from admitted resources or storage")
	}
	scratchLimit, err := filepack.PackedSizeLimit(scratchBytes, filepack.ScratchRole)
	if err != nil {
		return nil, err
	}
	result := &vm.BootSeedRestore{Manifest: append([]byte(nil), m.Config...)}
	var memory *checkpointMemorySource
	group, readCtx := errgroup.WithContext(ctx)
	group.SetLimit(2)
	stageErrors := make([]error, 3)
	group.Go(func() (err error) {
		defer func() { stageErrors[0] = err }()
		observed, err := p.ComputerObjects.Stat(readCtx, m.Memory.Digest)
		if err != nil {
			return checkpointObjectReadError(err)
		}
		if err := cas.RequireExact(observed, m.Memory); err != nil {
			return invalidCheckpoint(err)
		}
		memory, err = openCheckpointMemory(readCtx, p.ComputerRanges, p.CheckpointCipher, m.Memory, m.MemoryHeaderDigest, m.Purpose()+"/memory", memoryBytes)
		return err
	})
	for i, item := range []struct {
		role   string
		object cas.Descriptor
		limit  int64
		path   *string
	}{{"vm_state", m.VMState, vm.SnapshotStateLimit, &result.VMState}, {"scratch_disk", m.ScratchDisk, scratchLimit, &result.ScratchDisk}} {
		group.Go(func() (err error) {
			defer func() { stageErrors[i+1] = err }()
			*item.path, err = downloadCheckpointObject(readCtx, p.ComputerObjects, p.CheckpointCipher, directory, m.Purpose(),
				computercheckpoint.RuntimeObject{Role: item.role, Object: computercheckpoint.Object{Digest: item.object.Digest, SizeBytes: item.object.SizeBytes, MediaType: item.object.MediaType}}, item.limit)
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return nil, errors.Join(err, errors.Join(stageErrors...))
	}
	result.MemorySource = memory
	return result, nil
}

// A persistent selected-artifact read failure must not be selected again for
// this Computer. Transient store failures and unavailable wrapping keys retain
// their existing retry/pause semantics.
func bootSeedArtifactFailure(err error) bool {
	if errors.Is(err, ErrCheckpointKeyUnavailable) || errors.Is(err, cas.ErrUnavailable) {
		return false
	}
	if errors.Is(err, vm.ErrInvalidCheckpointContent) || errors.Is(err, errInvalidCheckpoint) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NotFound", "NoSuchKey", "AccessDenied", "Forbidden":
			return true
		}
	}
	return false
}
