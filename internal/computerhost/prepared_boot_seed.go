package computerhost

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/vm"
)

// A prepared private VM faults from fully retained local ciphertext. Adoption
// cannot inherit an optional network transfer or an expired preparation source.
// The leaf owns this directory through pager/process exclusion, not just start.
func (p *PreparedMachines) openPreparedBootSeed(ctx context.Context, directory string, m bootseed.Manifest, memoryBytes, scratchBytes int64) (*vm.BootSeedRestore, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.MemoryBytes != memoryBytes || m.ScratchBytes != scratchBytes || p.OptionalComputerObjects == nil {
		return nil, errors.New("prepared boot seed shape or store differs")
	}
	_, bound, err := memoryGeometry(memoryBytes)
	if err != nil {
		return nil, err
	}
	if m.Memory.SizeBytes > bound {
		return nil, errors.New("prepared memory exceeds its physical reservation")
	}
	local, err := cas.NewFile(filepath.Join(directory, "memory-ciphertext"))
	if err != nil {
		return nil, err
	}
	body, err := p.OptionalComputerObjects.Get(ctx, m.Memory.Digest)
	if err != nil {
		return nil, err
	}
	object, err := local.Put(ctx, m.Memory.MediaType, io.LimitReader(body, m.Memory.SizeBytes+1))
	err = errors.Join(err, body.Close())
	if err != nil {
		return nil, err
	}
	if err := cas.RequireExact(object, m.Memory); err != nil {
		return nil, err
	}
	seed, err := p.openBootSeedFrom(ctx, directory, m, memoryBytes, scratchBytes, p.OptionalComputerObjects, local)
	if err != nil {
		return nil, err
	}
	for offset := int64(0); offset < memoryBytes; {
		next, err := seed.MemorySource.ReadWindows(ctx, offset, func(int64, []byte) error { return nil })
		if err != nil {
			return nil, err
		}
		if next <= offset {
			return nil, errors.New("prepared memory verification did not advance")
		}
		offset = next
	}
	return seed, nil
}
