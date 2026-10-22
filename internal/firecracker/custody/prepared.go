package custody

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

// PreparedAdoption binds an immutable physical owner to the allocation that
// claimed it. Recovery retains the physical owner for cleanup and reports the
// allocation as quarantined until that cleanup succeeds.
type PreparedAdoption struct {
	Owner        vm.Owner `json:"owner"`
	AllocationID string   `json:"allocation_id"`
	WorkerEpoch  int64    `json:"worker_epoch"`
}

func (a PreparedAdoption) validate(owner vm.Owner) error {
	if a.Owner != owner || owner.Validate() != nil || a.WorkerEpoch <= 0 || !canonicalVMID(a.AllocationID) || a.AllocationID == owner.ID {
		return errors.New("prepared adoption identity is invalid")
	}
	return nil
}

// RecordPreparedAdoption must succeed before installing workload credentials
// or releasing the bootstrap gate. A partial record blocks recovery admission.
func (state StateRoot) RecordPreparedAdoption(a PreparedAdoption) error {
	owner, err := state.ReadOwner(a.Owner.ID)
	if err != nil {
		return err
	}
	if err := a.validate(owner); err != nil {
		return err
	}
	dir, err := openDirectory(state.Base, append(append([]string(nil), state.Components...), owner.ID)...)
	if err != nil {
		return err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), "prepared-adoption", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), "prepared-adoption"))
	err = json.NewEncoder(f).Encode(a)
	err = errors.Join(err, f.Sync(), f.Close(), dir.Sync())
	return err
}

// ReadPreparedAdoption returns an empty allocation for an ordinary or unclaimed
// owner. Malformed evidence is never treated as absence.
func (state StateRoot) ReadPreparedAdoption(owner vm.Owner) (PreparedAdoption, error) {
	actual, err := state.ReadOwner(owner.ID)
	if err != nil {
		return PreparedAdoption{}, err
	}
	if actual != owner {
		return PreparedAdoption{}, errors.New("prepared adoption owner changed")
	}
	dir, err := openDirectory(state.Base, append(append([]string(nil), state.Components...), owner.ID)...)
	if err != nil {
		return PreparedAdoption{}, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), "prepared-adoption", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return PreparedAdoption{}, nil
	}
	if err != nil {
		return PreparedAdoption{}, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), "prepared-adoption"))
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return PreparedAdoption{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1024 {
		return PreparedAdoption{}, errors.New("invalid prepared adoption record")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 1025))
	decoder.DisallowUnknownFields()
	var a PreparedAdoption
	if err := decoder.Decode(&a); err != nil {
		return PreparedAdoption{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return PreparedAdoption{}, errors.New("trailing prepared adoption data")
	}
	return a, a.validate(owner)
}
