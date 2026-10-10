package custody

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

// The configured base is trusted and resolved by the caller. Every component
// beneath it is opened relative to the preceding descriptor without symlinks.
func openDirectory(base string, components ...string) (*os.File, error) {
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open recovery directory", Path: base, Err: err}
	}
	name := base
	for _, component := range components {
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, `/\\`) {
			unix.Close(fd)
			return nil, errors.New("invalid recovery path component")
		}
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		name = filepath.Join(name, component)
		if openErr != nil {
			return nil, &os.PathError{Op: "open recovery directory", Path: name, Err: openErr}
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), name), nil
}

func ReadDirectory(base string, components ...string) ([]os.DirEntry, error) {
	dir, err := openDirectory(base, components...)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadDir(-1)
}

func (state StateRoot) ReadOwner(id string) (vm.Owner, error) {
	dir, err := openDirectory(state.Base, append(append([]string(nil), state.Components...), id)...)
	if err != nil {
		return vm.Owner{}, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), "owner", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return vm.Owner{}, err
	}
	marker := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), "owner"))
	defer marker.Close()
	info, err := marker.Stat()
	if err != nil {
		return vm.Owner{}, err
	}
	if !info.Mode().IsRegular() {
		return vm.Owner{}, errors.New("VM owner marker is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(marker, 256))
	if err != nil {
		return vm.Owner{}, err
	}
	owner, err := parseOwnerMarker(raw)
	if err == nil && owner.ID != id {
		err = errors.New("VM owner marker does not match its state directory")
	}
	return owner, err
}

type recoveryVMRoot struct {
	owner vm.Owner
	dir   *os.File // nil when this owner has no remaining jail root
	info  os.FileInfo
}

func closeRecoveryRoots(roots []recoveryVMRoot) {
	for _, root := range roots {
		if root.dir != nil {
			root.dir.Close()
		}
	}
}

func openRecoveryRoots(state StateRoot, jailerDir string) (_ []recoveryVMRoot, err error) {
	entries, err := ReadDirectory(state.Base, state.Components...)
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	var roots []recoveryVMRoot
	defer func() {
		if err != nil {
			closeRecoveryRoots(roots)
		}
	}()
	for _, entry := range entries {
		owner, readErr := state.ReadOwner(entry.Name())
		if readErr != nil {
			continue
		}
		root := recoveryVMRoot{owner: owner}
		root.dir, err = openDirectory(jailerDir, "firecracker", root.owner.ID, "root")
		if os.IsNotExist(err) {
			// A valid owner can be left before launch or after its jail was
			// removed. Absence still needs the process/network/cgroup barriers.
			err = nil
			roots = append(roots, root)
			continue
		}
		if err != nil {
			return nil, err
		}
		roots = append(roots, root)
		roots[len(roots)-1].info, err = root.dir.Stat()
		if err != nil {
			return nil, err
		}
		for _, previous := range roots[:len(roots)-1] {
			if previous.info != nil && os.SameFile(previous.info, roots[len(roots)-1].info) {
				return nil, errors.New("multiple VM owners identify the same jail root")
			}
		}
	}
	return roots, nil
}

func Processes(state StateRoot, jailerDir string) ([]Process, error) {
	return processesAt("/proc", state, jailerDir)
}

func processesAt(procDir string, state StateRoot, jailerDir string, expected ...vm.Owner) ([]Process, error) {
	roots, err := openRecoveryRoots(state, jailerDir)
	if err != nil {
		return nil, err
	}
	defer closeRecoveryRoots(roots)
	for _, owner := range expected {
		found := false
		for _, root := range roots {
			found = found || root.owner == owner
		}
		if !found {
			roots = append(roots, recoveryVMRoot{owner: owner})
		}
	}
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, err
	}
	var processes []Process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		processDir := filepath.Join(procDir, entry.Name())
		process, owned, err := inspectRecoveryProcess(processDir, pid, jailerDir, roots)
		if err != nil {
			if processDisappeared(processDir, err) {
				continue
			}
			return nil, err
		}
		if owned {
			processes = append(processes, process)
		}
	}
	return processes, nil
}

func inspectRecoveryProcess(processDir string, pid int, jailerDir string, roots []recoveryVMRoot) (Process, bool, error) {
	process := Process{PID: pid}
	cmdline, err := os.ReadFile(filepath.Join(processDir, "cmdline"))
	if err != nil {
		return process, false, fmt.Errorf("read process %d command line: %w", pid, err)
	}
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	switch filepath.Base(args[0]) {
	case "jailer":
		if filepath.Clean(commandFlag(args[1:], "--chroot-base-dir")) != filepath.Clean(jailerDir) || jailerDir == "" {
			return process, false, nil
		}
		process.ID = commandFlag(args[1:], "--id")
		if !canonicalVMID(process.ID) {
			process.Problem = fmt.Sprintf("owned jailer process has non-canonical --id %q", process.ID)
		}
		return process, true, nil
	case "firecracker":
		actual, err := os.Stat(filepath.Join(processDir, "root"))
		if err != nil {
			return process, false, fmt.Errorf("read process %d root identity: %w", pid, err)
		}
		if !actual.IsDir() {
			return process, false, fmt.Errorf("process %d root is not a directory", pid)
		}
		id := commandFlag(args[1:], "--id")
		for _, root := range roots {
			if root.info != nil && os.SameFile(root.info, actual) {
				process.ID = root.owner.ID
				if id != "" && id != process.ID {
					process.Problem = "Firecracker command identity contradicts its owned root"
				}
				return process, true, nil
			}
		}
		for _, root := range roots {
			if id == root.owner.ID {
				process.ID, process.Problem = id, "Firecracker command names an owner but its root identity does not match"
				return process, true, nil
			}
		}
	}
	return process, false, nil
}

// ESRCH identifies a dead task behind an open procfs file even if its PID was
// reused. ENOENT requires checking that the entire process disappeared.
func processDisappeared(processDir string, err error) bool {
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		_, statErr := os.Stat(processDir)
		return os.IsNotExist(statErr)
	}
	return false
}

func commandFlag(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
		if value, ok := strings.CutPrefix(arg, name+"="); ok {
			return value
		}
	}
	return ""
}

func canonicalVMID(name string) bool {
	return ids.Validate(name) == nil
}

func parseOwnerMarker(marker []byte) (vm.Owner, error) {
	lines := strings.Split(string(marker), "\n")
	if len(lines) != 3 || lines[2] != "" {
		return vm.Owner{}, errors.New("VM owner marker has invalid format")
	}
	owner := vm.Owner{Kind: vm.OwnerKind(lines[0]), ID: lines[1]}
	if err := owner.Validate(); err != nil {
		return vm.Owner{}, err
	}
	return owner, nil
}

// StateRoot keeps a trusted configured base separate from no-follow descendants.
// Worker supplies its work base and vms/guest; Connector supplies its state base.
type StateRoot struct {
	Base       string
	Components []string
}

type Process struct {
	PID     int
	ID      string
	Problem string
}

// MatchingPIDs fails if a process attributed to this owner has contradictory evidence.
func MatchingPIDs(state StateRoot, jailerDir string, owner vm.Owner) ([]int, error) {
	processes, err := processesAt("/proc", state, jailerDir, owner)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, process := range processes {
		if process.ID != owner.ID {
			continue
		}
		if process.Problem != "" {
			return nil, errors.New(process.Problem)
		}
		pids = append(pids, process.PID)
	}
	return pids, nil
}
