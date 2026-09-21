package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
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
	if (source.Seed == nil) == (source.Disk == nil) {
		return errors.New("computer source requires exactly one seed or disk")
	}
	if source.Seed != nil {
		if source.Seed.Profile != computer.SeedProfile || target.Source.Restore != nil {
			return errors.New("invalid initializing computer source")
		}
		return (computer.SeedArtifact{Object: computerObject(source.Seed.Object), LogicalBytes: source.LogicalBytes}).Validate(source.LogicalBytes)
	}
	return (computer.DiskArtifact{Object: computerObject(*source.Disk), LogicalBytes: source.LogicalBytes}).Validate(source.LogicalBytes)
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
