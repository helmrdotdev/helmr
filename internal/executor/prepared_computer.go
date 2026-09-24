package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func validateComputerPreparationSource(target workerapi.RuntimeReconcileTarget) error {
	if err := ids.Validate(target.ID); err != nil {
		return fmt.Errorf("runtime preparation identity: %w", err)
	}
	if target.WorkerEpoch <= 0 || target.DesiredVersion <= 0 {
		return errors.New("runtime preparation fence is required")
	}
	if err := ids.Validate(target.Source.WorkspaceID); err != nil {
		return fmt.Errorf("computer identity: %w", err)
	}
	source := target.Source.Computer
	if source == nil {
		return errors.New("runtime computer source is required")
	}
	if err := ids.Validate(source.VersionID); err != nil {
		return fmt.Errorf("computer version: %w", err)
	}
	if source.LogicalBytes != computer.SeedCapacity || target.Source.ReservedDiskMiB != source.LogicalBytes/mebibyte {
		return errors.New("computer capacity does not match runtime reservation")
	}
	if source.Seed != nil {
		if source.Seed.Profile != computer.SeedProfile || target.Source.Restore != nil {
			return errors.New("invalid initializing computer source")
		}
		return (computer.SeedArtifact{Object: computerObject(source.Seed.Object), LogicalBytes: source.LogicalBytes}).Validate(source.LogicalBytes)
	}
	return nil // Continuation is resolved by the fenced source/key broker, never a disk artifact.
}

func computerObject(object workerapi.CASObject) cas.Descriptor {
	return cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
}

func computerStagingKey(id string, epoch int64) capacity.Key {
	return capacity.Key{Kind: "computer-staging", ID: strings.TrimSpace(id), Epoch: epoch}
}

func (p *PreparedRuntimePool) computerPreparationDirectory(id string, epoch int64) string {
	root := strings.TrimSpace(p.TempDir)
	if root == "" {
		root = os.TempDir()
	}
	return filepath.Join(root, "computer-"+id+"-"+strconv.FormatInt(epoch, 10))
}

// Retain before attachment can fail. Close is safe both before binding and after
// connector cleanup, but refuses release while a bound consumer may still live.
func (p *PreparedRuntimePool) retainComputerDevice(id string, epoch int64, device vm.ComputerDevice) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.computerDevices == nil {
		p.computerDevices = make(map[preparedRuntimeRef]vm.ComputerDevice)
	}
	p.computerDevices[preparedRuntimeRef{id: id, epoch: epoch}] = device
}
func (p *PreparedRuntimePool) releaseComputerDevice(id string, epoch int64) error {
	if p == nil {
		return nil
	}
	ref := preparedRuntimeRef{id: id, epoch: epoch}
	p.mu.Lock()
	device := p.computerDevices[ref]
	p.mu.Unlock()
	if device == nil {
		// A restarted Worker has no process-local owner. The helper deliberately
		// quarantines on parent death; filesystem evidence must survive until an
		// external reconciler establishes exclusive release.
		if _, err := os.Lstat(filepath.Join(p.computerPreparationDirectory(id, epoch), "config.json")); err == nil {
			return errors.New("computer attachment requires external reconciliation after owner loss")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := device.Close(ctx); err != nil {
		return err
	}
	return nil
}
