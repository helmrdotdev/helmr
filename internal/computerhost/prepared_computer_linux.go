//go:build linux

package computerhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/nbd"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Preparation has one storage format: a retained authenticated disk version.
// The seed is decoded only for initial publication and never used for recovery.
func (p *PreparedMachines) prepareComputerDevice(ctx context.Context, target workerapi.InstanceReconcileTarget) (vm.ComputerDevice, error) {
	if !filepath.IsAbs(p.ComputerHelper) || len(p.ComputerDevices) == 0 {
		return nil, errors.New("computer helper and explicit device allowlist required")
	}
	local, err := p.prepareComputerVersion(ctx, target)
	if err != nil {
		return nil, err
	}
	dir := p.computerPreparationDirectory(target.ID, target.WorkerEpoch)
	device, err := disk.AttachDevice(ctx, local, nbd.Config{Helper: p.ComputerHelper, Devices: p.ComputerDevices, Arena: dir, Socket: filepath.Join(dir, "nbd.sock"), Size: target.Source.Computer.LogicalBytes})
	if device != nil {
		p.retainComputerDevice(target.ID, target.WorkerEpoch, device)
	}
	return device, err
}

func (p *PreparedMachines) prepareComputerVersion(ctx context.Context, target workerapi.InstanceReconcileTarget) (*disk.LocalVersion, error) {
	if err := validateComputerPreparationSource(target); err != nil {
		return nil, err
	}
	if p.CAS == nil || p.ComputerRanges == nil || p.ComputerPreparation == nil || p.Reservations == nil || p.ComputerStagingBytes <= 0 {
		return nil, errors.New("computer preparation requires storage, source authority and bounded staging admission")
	}
	created, err := p.Reservations.Reserve(computerStagingKey(target.ID, target.WorkerEpoch), reservation.Vector{HostDiskBytes: p.ComputerStagingBytes})
	if errors.Is(err, reservation.ErrCapacityExceeded) || err == nil && !created {
		return nil, errPreparedMachineCapacityBusy
	}
	if err != nil {
		return nil, err
	}
	dir := p.computerPreparationDirectory(target.ID, target.WorkerEpoch)
	// Cleanup belongs to the Instance, including partially created device evidence.
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
		return nil, errors.New("computer disk version differs from instance reservation")
	}
	if material.VersionID != source.VersionID || material.Root.LogicalBytes != source.LogicalBytes || len(material.Keys) == 0 {
		return nil, errors.New("computer source differs from instance reservation")
	}
	keys := make(map[string][]byte, len(material.Keys))
	scope := material.Keys[0].Scope
	for _, key := range material.Keys {
		if key.Scope != scope {
			return nil, errors.New("computer source key scope mismatch")
		}
		keys[key.ID] = key.Key
	}
	if _, err := disk.OpenVersion(ctx, p.ComputerRanges, scope, keys, material.Root, source.LogicalBytes); err != nil {
		return nil, disk.PublishedSourceFailure(err)
	}
	return disk.CreateLocalVersion(ctx, disk.LocalVersionConfig{Directory: filepath.Join(dir, "version"), Base: material.Root, BaseSource: p.ComputerRanges, Scope: scope, ActiveKey: material.WriteKeyID, Keys: keys, DirtyBlocks: 256, StagedBytes: p.ComputerStagingBytes, PackLimit: blockformat.MinPackLimit})
}

func (p *PreparedMachines) publishComputerSeed(ctx context.Context, target workerapi.InstanceReconcileTarget, dir string) (retErr error) {
	if p.ComputerObjects == nil {
		return errors.New("computer object publication required")
	}
	source := target.Source.Computer
	var key workerapi.ComputerKeyMaterial
	preparationStarted := time.Now()
	for {
		preparation, err := p.ComputerPreparation.PrepareComputerSeed(ctx, workerapi.PrepareComputerSeedRequest{ComputerInstanceID: target.ID, DesiredVersion: target.DesiredVersion})
		if err != nil {
			return err
		}
		switch preparation.Status {
		case "ready":
			p.logInfo("computer seed phase", "computer_instance_id", target.ID, "phase", "adopt", "duration_ms", time.Since(preparationStarted).Milliseconds())
			return nil
		case "waiting":
			if err := sleepWithContext(ctx, time.Second); err != nil {
				return err
			}
			continue
		case "convert":
			if preparation.Key == nil {
				return errors.New("seed conversion has no key")
			}
			key = *preparation.Key
		default:
			return errors.New("invalid seed preparation state")
		}
		break
	}
	defer clear(key.Key)
	p.logInfo("computer seed phase", "computer_instance_id", target.ID, "phase", "claim", "duration_ms", time.Since(preparationStarted).Milliseconds())
	path := filepath.Join(dir, "seed.raw")
	phaseStarted := time.Now()
	if err := (disk.SeedStore{CAS: p.CAS}).Decode(ctx, disk.SeedArtifact{Object: computerObject(source.Seed.Object), LogicalBytes: source.LogicalBytes}, path, source.LogicalBytes); err != nil {
		return err
	}
	p.logInfo("computer seed phase", "computer_instance_id", target.ID, "phase", "decode", "duration_ms", time.Since(phaseStarted).Milliseconds())
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close(), os.Remove(path)) }()
	phaseStarted = time.Now()
	candidate, err := disk.CaptureInitialVersion(ctx, disk.VersionCapture{Disk: file, Capacity: source.LogicalBytes, StagingParent: dir, Scope: key.Scope, KeyID: key.ID, Key: key.Key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: p.ComputerStagingBytes, MaxObjects: 1 << 20})
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, candidate.Close()) }()
	p.logInfo("computer seed phase", "computer_instance_id", target.ID, "phase", "capture", "duration_ms", time.Since(phaseStarted).Milliseconds())
	publisher, err := NewInitialVersionPublisher(p.ComputerPreparation, p.ComputerObjects, target.ID, target.DesiredVersion)
	if err != nil {
		return err
	}
	phaseStarted = time.Now()
	locator, err := candidate.Publish(ctx, publisher)
	if err != nil {
		return err
	}
	root, err := disk.NewVersionRoot(locator, source.LogicalBytes)
	if err != nil {
		return err
	}
	published, err := p.ComputerPreparation.PublishInitialComputerVersion(ctx, workerapi.InitialComputerVersionRequest{ComputerInstanceID: target.ID, DesiredVersion: target.DesiredVersion, Root: root, Config: source.Config})
	if err != nil {
		return fmt.Errorf("publish initial computer version: %w", err)
	}
	if published.ComputerID != target.Source.ComputerID || published.VersionID != source.VersionID {
		return errors.New("published computer version identity mismatch")
	}
	p.logInfo("computer seed phase", "computer_instance_id", target.ID, "phase", "publish", "duration_ms", time.Since(phaseStarted).Milliseconds())
	return nil
}

// Retain before attachment can fail. Close is safe both before binding and after
// backend cleanup, but refuses release while a bound consumer may still live.
func (p *PreparedMachines) retainComputerDevice(id string, epoch int64, device vm.ComputerDevice) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.computerDevices == nil {
		p.computerDevices = make(map[preparedMachineRef]vm.ComputerDevice)
	}
	p.computerDevices[preparedMachineRef{id: id, epoch: epoch}] = device
}
