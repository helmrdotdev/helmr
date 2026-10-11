package computerhost

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type preparationStage struct {
	directory string
	disk      *os.File
	images    *programSnapshots
	drives    []vm.ReadOnlyDrive
}

// Closing requires proof that the backend no longer uses these exact inodes.
func (s *preparationStage) close() error {
	if s == nil {
		return nil
	}
	if s.images != nil {
		if err := s.images.close(); err != nil {
			return err
		}
		s.images = nil
	}
	if s.disk != nil {
		if err := s.disk.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			return err
		}
		s.disk = nil
	}
	if s.directory != "" {
		if err := os.RemoveAll(s.directory); err != nil {
			return err
		}
		s.directory = ""
	}
	return nil
}

func (p *PreparedMachines) preparationDirectory(identity workerapi.AllocationIdentity) (*preparationStage, error) {
	if !filepath.IsAbs(p.TempDir) {
		return nil, errors.New("preparation requires an absolute private staging directory")
	}
	if err := os.MkdirAll(p.TempDir, 0700); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(p.TempDir, "preparation-"+identity.InstanceID+"-")
	if err != nil {
		return nil, err
	}
	return &preparationStage{directory: directory}, nil
}
