package computerhost

import (
	"context"
	"errors"

	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func computerObject(object workerapi.CASObject) cas.Descriptor {
	return cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
}

func (p *PreparedMachines) computerPreparationDirectory(id string, epoch int64) string {
	root := strings.TrimSpace(p.TempDir)
	if root == "" {
		root = os.TempDir()
	}
	return filepath.Join(root, "computer-"+id+"-"+strconv.FormatInt(epoch, 10))
}

func (p *PreparedMachines) releaseComputerDevice(id string, epoch int64) error {
	if p == nil {
		return nil
	}
	ref := preparedMachineRef{id: id, epoch: epoch}
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
