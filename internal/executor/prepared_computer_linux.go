//go:build linux

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// prepareComputerDisk runs under the runtime's working-disk reservation. It
// returns no bootable file until the exact initial ciphertext is committed, or
// the reserved continuation disk is completely authenticated and decoded.
func (p *PreparedRuntimePool) prepareComputerDisk(ctx context.Context, target workerapi.RuntimeReconcileTarget) (_ *os.File, cleanup func() error, retErr error) {
	if err := validateComputerPreparationSource(target); err != nil {
		return nil, nil, err
	}
	if p.CAS == nil || p.CheckpointEncryptor == nil || p.Capacity == nil {
		return nil, nil, errors.New("computer preparation storage, encryption and capacity are required")
	}
	source := target.Source.Computer
	store := computer.DiskStore{CAS: p.CAS, Cipher: p.CheckpointEncryptor}
	stagingKey := computerStagingKey(target.ID, target.WorkerEpoch)
	if source.Seed != nil {
		if p.ComputerObjects == nil || p.ComputerInitializations == nil {
			return nil, nil, errors.New("computer initial publication dependencies are required")
		}
		limit, err := store.CaptureSizeLimit(source.LogicalBytes)
		if err != nil {
			return nil, nil, err
		}
		created, err := p.Capacity.Reserve(stagingKey, capacity.Vector{GuestEphemeralDiskBytes: limit})
		if errors.Is(err, capacity.ErrCapacityExceeded) || err == nil && !created {
			return nil, nil, errPreparedRuntimeCapacityBusy
		}
		if err != nil {
			return nil, nil, err
		}
	}
	dir := p.computerPreparationDirectory(target.ID, target.WorkerEpoch)
	// Exclusive runtime-owned directory. An interrupted previous preparation
	// must be reclaimed, never silently replaced and reencrypted under its fence.
	if err := os.Mkdir(dir, 0700); err != nil {
		if source.Seed != nil {
			err = errors.Join(err, p.Capacity.Release(stagingKey))
		}
		return nil, nil, err
	}
	defer func() {
		if retErr != nil {
			removeErr := os.RemoveAll(dir)
			retErr = errors.Join(retErr, removeErr)
			if removeErr == nil && source.Seed != nil {
				retErr = errors.Join(retErr, p.Capacity.Release(stagingKey))
			}
		}
	}()
	path := filepath.Join(dir, "computer.raw")
	if source.Seed != nil {
		candidate, err := store.Initialize(ctx, target.Source.WorkspaceID, computer.Seed{
			Artifact: computer.SeedArtifact{Object: computerObject(source.Seed.Object), LogicalBytes: source.LogicalBytes}, Config: source.Config,
		}, path, dir, source.LogicalBytes)
		if err != nil {
			return nil, nil, err
		}
		defer func() { retErr = errors.Join(retErr, candidate.Disk.Close()) }()
		artifact := candidate.Disk.Artifact()
		config, err := json.Marshal(candidate.Config)
		if err != nil {
			return nil, nil, err
		}
		request := workerapi.ComputerInitializationRequest{RuntimeInstanceID: target.ID, DesiredVersion: target.DesiredVersion,
			Disk:         workerapi.CASObject{Digest: artifact.Object.Digest, SizeBytes: artifact.Object.SizeBytes, MediaType: artifact.Object.MediaType},
			LogicalBytes: artifact.LogicalBytes, InitialConfig: config}
		registered, err := p.ComputerInitializations.RegisterComputerInitialization(ctx, request)
		if err != nil {
			return nil, nil, fmt.Errorf("register initial computer disk: %w", err)
		}
		if err := validateComputerInitializationReceipt(target, registered); err != nil {
			return nil, nil, err
		}
		if registered.Status == "registered" {
			if err := candidate.Disk.Upload(ctx, p.ComputerObjects); err != nil {
				return nil, nil, fmt.Errorf("upload initial computer disk: %w", err)
			}
			published, err := p.ComputerInitializations.PublishComputerInitialization(ctx, request)
			if err != nil {
				return nil, nil, fmt.Errorf("publish initial computer disk: %w", err)
			}
			if err := validateComputerInitializationReceipt(target, published); err != nil {
				return nil, nil, err
			}
			if published.Status != "consumed" || published.ID != registered.ID {
				return nil, nil, errors.New("initial computer publication has no matching committed receipt")
			}
		}
		if err := candidate.Disk.Close(); err != nil {
			return nil, nil, err
		}
		if err := p.Capacity.Release(stagingKey); err != nil {
			return nil, nil, err
		}
	} else {
		if err := store.Restore(ctx, target.Source.WorkspaceID, computer.DiskArtifact{Object: computerObject(*source.Disk), LogicalBytes: source.LogicalBytes}, path, source.LogicalBytes); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	return file, func() error { return errors.Join(file.Close(), os.RemoveAll(dir)) }, nil
}

func validateComputerInitializationReceipt(target workerapi.RuntimeReconcileTarget, receipt workerapi.ComputerInitializationResponse) error {
	if receipt.ID == "" || receipt.ComputerID != target.Source.WorkspaceID || receipt.VersionID != target.Source.Computer.VersionID {
		return errors.New("initial computer receipt identity mismatch")
	}
	switch receipt.Status {
	case "registered":
		if receipt.ArtifactID != "" {
			return errors.New("uncommitted computer receipt has an artifact")
		}
	case "consumed":
		if receipt.ArtifactID == "" {
			return errors.New("committed computer receipt has no artifact")
		}
	default:
		return errors.New("initial computer candidate is not publishable")
	}
	return nil
}
