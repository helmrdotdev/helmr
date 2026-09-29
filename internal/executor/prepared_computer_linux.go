//go:build linux

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/nbd"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Preparation has one storage format: a retained authenticated generation.
// The seed is decoded only for initial publication and never used for recovery.
func (p *PreparedRuntimePool) prepareComputerDevice(ctx context.Context, target workerapi.RuntimeReconcileTarget) (vm.ComputerDevice, error) {
	if !filepath.IsAbs(p.ComputerHelper) || len(p.ComputerDevices) == 0 {
		return nil, errors.New("computer helper and explicit device allowlist required")
	}
	generation, err := p.prepareComputerGeneration(ctx, target)
	if err != nil {
		return nil, err
	}
	dir := p.computerPreparationDirectory(target.ID, target.WorkerEpoch)
	device, err := disk.AttachDevice(ctx, generation, nbd.Config{Helper: p.ComputerHelper, Devices: p.ComputerDevices, Arena: dir, Socket: filepath.Join(dir, "nbd.sock"), Size: target.Source.Computer.LogicalBytes})
	if device != nil {
		p.retainComputerDevice(target.ID, target.WorkerEpoch, device)
	}
	return device, err
}

func (p *PreparedRuntimePool) prepareComputerGeneration(ctx context.Context, target workerapi.RuntimeReconcileTarget) (*disk.LocalGeneration, error) {
	if err := validateComputerPreparationSource(target); err != nil {
		return nil, err
	}
	if p.CAS == nil || p.ComputerRanges == nil || p.ComputerPreparation == nil || p.Reservations == nil || p.ComputerStagingBytes <= 0 {
		return nil, errors.New("computer preparation requires storage, source authority and bounded staging admission")
	}
	created, err := p.Reservations.Reserve(computerStagingKey(target.ID, target.WorkerEpoch), reservation.Vector{GuestEphemeralDiskBytes: p.ComputerStagingBytes})
	if errors.Is(err, reservation.ErrCapacityExceeded) || err == nil && !created {
		return nil, errPreparedRuntimeCapacityBusy
	}
	if err != nil {
		return nil, err
	}
	dir := p.computerPreparationDirectory(target.ID, target.WorkerEpoch)
	// Cleanup belongs to the Runtime, including partially created device evidence.
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	source := target.Source.Computer
	if source.Seed != nil {
		if err := p.publishComputerSeed(ctx, target, dir); err != nil {
			return nil, err
		}
	}
	material, err := p.ComputerPreparation.ComputerSource(ctx, workerapi.ComputerSourceRequest{ComputerInstanceID: target.ID, DesiredVersion: target.DesiredVersion})
	if err != nil {
		return nil, err
	}
	defer material.Clear()
	if source.Seed == nil && (source.Root == nil || material.Root != *source.Root) {
		return nil, errors.New("computer generation differs from runtime reservation")
	}
	if material.VersionID != source.VersionID || material.Root.LogicalBytes != source.LogicalBytes || len(material.Keys) == 0 {
		return nil, errors.New("computer source differs from runtime reservation")
	}
	keys := make(map[string][]byte, len(material.Keys))
	scope := material.Keys[0].Scope
	for _, key := range material.Keys {
		if key.Scope != scope {
			return nil, errors.New("computer source key scope mismatch")
		}
		keys[key.ID] = key.Key
	}
	if _, err := disk.OpenGeneration(ctx, p.ComputerRanges, scope, keys, material.Root, source.LogicalBytes); err != nil {
		return nil, disk.PublishedSourceFailure(err)
	}
	return disk.CreateLocalGeneration(ctx, disk.LocalGenerationConfig{Directory: filepath.Join(dir, "generation"), Base: material.Root, BaseSource: p.ComputerRanges, Scope: scope, ActiveKey: material.WriteKeyID, Keys: keys, DirtyBlocks: 256, StagedBytes: p.ComputerStagingBytes, PackLimit: blockformat.MinPackLimit})
}

func (p *PreparedRuntimePool) publishComputerSeed(ctx context.Context, target workerapi.RuntimeReconcileTarget, dir string) (retErr error) {
	if p.ComputerObjects == nil {
		return errors.New("computer object publication required")
	}
	source := target.Source.Computer
	path := filepath.Join(dir, "seed.raw")
	if err := (disk.SeedStore{CAS: p.CAS}).Decode(ctx, disk.SeedArtifact{Object: computerObject(source.Seed.Object), LogicalBytes: source.LogicalBytes}, path, source.LogicalBytes); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close(), os.Remove(path)) }()
	key, err := p.ComputerPreparation.InitialComputerKey(ctx, workerapi.InitialComputerKeyRequest{ComputerInstanceID: target.ID, DesiredVersion: target.DesiredVersion})
	if err != nil {
		return err
	}
	defer clear(key.Key)
	candidate, err := disk.CaptureInitialGeneration(ctx, disk.GenerationCapture{Disk: file, Capacity: source.LogicalBytes, StagingParent: dir, Scope: key.Scope, KeyID: key.ID, Key: key.Key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: p.ComputerStagingBytes, MaxObjects: 1 << 20})
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, candidate.Close()) }()
	publisher, err := NewInitialGenerationPublisher(p.ComputerPreparation, p.ComputerObjects, target.ID, target.DesiredVersion)
	if err != nil {
		return err
	}
	locator, err := candidate.Publish(ctx, publisher)
	if err != nil {
		return err
	}
	root, err := disk.NewGenerationRoot(locator, source.LogicalBytes)
	if err != nil {
		return err
	}
	published, err := p.ComputerPreparation.PublishInitialComputerGeneration(ctx, workerapi.InitialComputerGenerationRequest{ComputerInstanceID: target.ID, DesiredVersion: target.DesiredVersion, Root: root, Config: source.Config})
	if err != nil {
		return fmt.Errorf("publish initial computer generation: %w", err)
	}
	if published.ComputerID != target.Source.ComputerID || published.VersionID != source.VersionID {
		return errors.New("published computer generation identity mismatch")
	}
	return nil
}

// Retain before attachment can fail. Close is safe both before binding and after
// backend cleanup, but refuses release while a bound consumer may still live.
func (p *PreparedRuntimePool) retainComputerDevice(id string, epoch int64, device vm.ComputerDevice) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.computerDevices == nil {
		p.computerDevices = make(map[preparedRuntimeRef]vm.ComputerDevice)
	}
	p.computerDevices[preparedRuntimeRef{id: id, epoch: epoch}] = device
}
