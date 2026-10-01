//go:build linux

package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/sirupsen/logrus"
)

func TestJailRootPath(t *testing.T) {
	cfg := (Config{
		FirecrackerPath:     "/usr/bin/firecracker",
		JailerChrootBaseDir: "/var/lib/helmr/jailer",
	}).WithDefaults()
	got := jailRootPath(cfg, "vm-1")
	want := "/var/lib/helmr/jailer/firecracker/vm-1/root"
	if got != want {
		t.Fatalf("jail root = %q, want %q", got, want)
	}
}

func TestLinkIntoJailSetsOwnerAndMode(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to verify chown")
	}
	source := filepath.Join(t.TempDir(), "snapshot.mem")
	if err := os.WriteFile(source, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := linkIntoJailForVMM(source, root, "snapshot.mem", os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "snapshot.mem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWithJailedRestoreFilesLinksComputerAndPreservesSnapshotInodes(t *testing.T) {
	chrootBase := t.TempDir()
	vmID := "vm-1"
	root := filepath.Join(chrootBase, "firecracker", vmID, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	rootfsPath := filepath.Join(sourceDir, "rootfs.squashfs")
	scratchDiskPath := filepath.Join(sourceDir, "restored-scratch.ext4")
	computerDiskPath := filepath.Join(sourceDir, "9270959e49b0181ace5338d3acce327260b9e46d6f3827402dfca962a5189126.ext4")
	memoryPath := filepath.Join(sourceDir, "checkpoint.mem")
	statePath := filepath.Join(sourceDir, "checkpoint.vmstate")
	for _, path := range []string{rootfsPath, scratchDiskPath, computerDiskPath, memoryPath, statePath} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	sdkMachine := &firecracker.Machine{
		Cfg: firecracker.Config{
			JailerCfg: &firecracker.JailerConfig{
				ExecFile:      "/usr/bin/firecracker",
				ChrootBaseDir: chrootBase,
				ID:            vmID,
				UID:           firecracker.Int(os.Getuid()),
				GID:           firecracker.Int(os.Getgid()),
			},
			Drives: []models.Drive{{
				DriveID:    firecracker.String("rootfs"),
				PathOnHost: firecracker.String(rootfsPath),
			}, {
				DriveID:    firecracker.String("scratch"),
				PathOnHost: firecracker.String(scratchDiskPath),
			}, {
				DriveID:    firecracker.String("computer"),
				PathOnHost: firecracker.String(computerDiskPath),
			}},
			Snapshot: firecracker.SnapshotConfig{},
		},
		Handlers: firecracker.Handlers{
			FcInit: firecracker.HandlerList{}.Append(firecracker.Handler{
				Name: firecracker.CreateLogFilesHandlerName,
				Fn: func(context.Context, *firecracker.Machine) error {
					return nil
				},
			}),
		},
	}
	firecracker.WithLogger(logrus.NewEntry(logrus.New()))(sdkMachine)
	opt := withJailedRestoreFiles(rootfsPath, scratchDiskPath, computerDiskPath, memoryPath, statePath)
	opt(sdkMachine)
	if err := sdkMachine.Handlers.FcInit.Run(context.Background(), sdkMachine); err != nil {
		t.Fatal(err)
	}

	if got := firecracker.StringValue(sdkMachine.Cfg.Drives[0].PathOnHost); got != filepath.Base(rootfsPath) {
		t.Fatalf("rootfs drive path = %q", got)
	}
	if got := firecracker.StringValue(sdkMachine.Cfg.Drives[1].PathOnHost); got != scratchDiskName {
		t.Fatalf("scratch drive path = %q", got)
	}
	computerName := "computer.ext4"
	if got := firecracker.StringValue(sdkMachine.Cfg.Drives[2].PathOnHost); got != computerName {
		t.Fatalf("computer drive path = %q", got)
	}
	for _, name := range []string{filepath.Base(rootfsPath), scratchDiskName, computerName, filepath.Base(memoryPath), filepath.Base(statePath)} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("expected %s linked into jail: %v", name, err)
		}
	}
	for source, name := range map[string]string{computerDiskPath: "computer.ext4", memoryPath: filepath.Base(memoryPath), statePath: filepath.Base(statePath)} {
		before, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		jailed, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, jailed) {
			t.Fatalf("%s was copied instead of retaining inode", name)
		}
	}

}

func TestRestoreReadOnlyDrivePathsSatisfySDKValidationAndBecomeJailedNames(t *testing.T) {
	ownerDir := t.TempDir()
	sourceDir := t.TempDir()
	programRuntimeSource := filepath.Join(sourceDir, "runtime-source")
	programSource := filepath.Join(sourceDir, "program-source")
	for path, contents := range map[string]string{
		programRuntimeSource: "runtime",
		programSource:        "program",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	drives := []vm.ReadOnlyDrive{
		{ID: vm.ProgramRuntimeDrive, Source: fileLinkReadOnlyDriveSource{path: programRuntimeSource}},
		{ID: vm.ProgramDrive, Source: fileLinkReadOnlyDriveSource{path: programSource}},
	}
	paths, err := prepareRestoreReadOnlyDrivePaths(
		ownerDir,
		drives,
		os.Getuid(),
		os.Getgid(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range readOnlyDriveOrder {
		want := filepath.Join(ownerDir, readOnlyDriveName(id))
		if paths[id] != want || !filepath.IsAbs(paths[id]) {
			t.Fatalf("restore path for %q = %q, want absolute %q", id, paths[id], want)
		}
		info, err := os.Lstat(paths[id])
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("restore path for %q is not regular", id)
		}
	}

	machineFiles := t.TempDir()
	rootfsPath := filepath.Join(machineFiles, "rootfs.squashfs")
	scratchPath := filepath.Join(machineFiles, "scratch.ext4")
	memoryPath := filepath.Join(machineFiles, "memory.mem")
	statePath := filepath.Join(machineFiles, "state.vmstate")
	kernelPath := filepath.Join(machineFiles, "vmlinux")
	for _, path := range []string{rootfsPath, scratchPath, memoryPath, statePath, kernelPath} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := firecracker.Config{
		SocketPath: filepath.Join(machineFiles, "api.sock"),
		Drives: runtimeDrivesWithComputer(
			rootfsPath,
			scratchPath,
			"",
			drives,
			paths,
		),
		Snapshot: firecracker.SnapshotConfig{
			MemFilePath:  memoryPath,
			SnapshotPath: statePath,
		},
	}
	if err := cfg.ValidateLoadSnapshot(); err != nil {
		t.Fatalf("SDK snapshot validation rejected owner-scoped drive paths: %v", err)
	}

	chrootBase := t.TempDir()
	vmID := "restore-vm"
	root := filepath.Join(chrootBase, "firecracker", vmID, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	strategy := sealedDriveChrootStrategy{kernelImagePath: kernelPath, drives: drives}
	sdkMachine, err := firecracker.NewMachine(
		context.Background(),
		firecracker.Config{
			VMID:            vmID,
			SocketPath:      filepath.Join(machineFiles, "api.sock"),
			KernelImagePath: kernelPath,
			JailerCfg: &firecracker.JailerConfig{
				ExecFile:       "/usr/bin/firecracker",
				ChrootBaseDir:  chrootBase,
				ID:             vmID,
				UID:            firecracker.Int(os.Getuid()),
				GID:            firecracker.Int(os.Getgid()),
				NumaNode:       firecracker.Int(0),
				ChrootStrategy: strategy,
			},
			Drives: cfg.Drives,
		},
		withSnapshotRestore(memoryPath, statePath),
		withJailedRestoreFiles(rootfsPath, scratchPath, "", memoryPath, statePath),
		withRestoreSealedDrives(strategy),
		func(sdkMachine *firecracker.Machine) {
			for _, name := range []string{
				firecracker.SetupNetworkHandlerName,
				firecracker.StartVMMHandlerName,
				firecracker.CreateLogFilesHandlerName,
				firecracker.BootstrapLoggingHandlerName,
				firecracker.LoadSnapshotHandlerName,
			} {
				sdkMachine.Handlers.FcInit = sdkMachine.Handlers.FcInit.Swap(firecracker.Handler{
					Name: name,
					Fn:   func(context.Context, *firecracker.Machine) error { return nil },
				})
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := sdkMachine.Handlers.FcInit.Run(context.Background(), sdkMachine); err != nil {
		t.Fatal(err)
	}
	for _, drive := range sdkMachine.Cfg.Drives {
		id := firecracker.StringValue(drive.DriveID)
		if id != vm.ProgramRuntimeDrive && id != vm.ProgramDrive {
			continue
		}
		want := readOnlyDriveName(id)
		if got := firecracker.StringValue(drive.PathOnHost); got != want {
			t.Fatalf("jailed path for %q = %q, want %q", id, got, want)
		}
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Fatalf("jailed drive %q is absent: %v", id, err)
		}
	}

	if err := os.RemoveAll(ownerDir); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("owner projection %q survived cleanup: %v", path, err)
		}
	}
}

func TestPrepareRestoreReadOnlyDrivePathsFailsClosed(t *testing.T) {
	drives := []vm.ReadOnlyDrive{{
		ID:     vm.ProgramRuntimeDrive,
		Source: failingReadOnlyDriveSource{err: errors.New("source changed")},
	}}
	if _, err := prepareRestoreReadOnlyDrivePaths(
		t.TempDir(),
		drives,
		os.Getuid(),
		os.Getgid(),
	); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("error = %v, want sealed source failure", err)
	}
	if _, err := prepareRestoreReadOnlyDrivePaths(
		"relative-owner",
		drives,
		os.Getuid(),
		os.Getgid(),
	); err == nil {
		t.Fatal("relative owner directory should fail")
	}
}

func TestRuntimeDrivesIncludeSealedReadOnlyDrives(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	drives := runtimeDrivesWithComputer(
		"/rootfs.squashfs",
		"/scratch.ext4",
		"",
		[]vm.ReadOnlyDrive{{ID: vm.ProgramDrive, Source: source}},
		nil,
	)
	if len(drives) != 3 {
		t.Fatalf("drive count = %d, want 3", len(drives))
	}
	if got := firecracker.StringValue(drives[2].DriveID); got != vm.ProgramDrive {
		t.Fatalf("program drive id = %q", got)
	}
	if got := firecracker.StringValue(drives[2].PathOnHost); got != "program.squashfs" {
		t.Fatalf("program drive path = %q", got)
	}
	if !firecracker.BoolValue(drives[2].IsReadOnly) {
		t.Fatal("program drive should be read-only")
	}
}

func TestRuntimeDrivesUseFixedProgramOrder(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	drives := runtimeDrivesWithComputer(
		"/rootfs.squashfs",
		"/scratch.ext4",
		"/computer.ext4",
		[]vm.ReadOnlyDrive{
			{ID: vm.ProgramDrive, Source: source},
			{ID: vm.ProgramRuntimeDrive, Source: source},
		},
		nil,
	)
	want := []string{
		"rootfs",
		"scratch",
		"computer",
		vm.ProgramRuntimeDrive,
		vm.ProgramDrive,
	}
	if len(drives) != len(want) {
		t.Fatalf("drive count = %d, want %d", len(drives), len(want))
	}
	for index, drive := range drives {
		if got := firecracker.StringValue(drive.DriveID); got != want[index] {
			t.Fatalf("drive %d ID = %q, want %q", index, got, want[index])
		}
	}
}

func TestSealedDriveChrootStrategySeparatesSourceCapabilities(t *testing.T) {
	chrootBase := t.TempDir()
	vmID := "vm-1"
	root := filepath.Join(chrootBase, "firecracker", vmID, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceDirectory := t.TempDir()
	kernelPath := filepath.Join(sourceDirectory, "vmlinux")
	rootfsPath := filepath.Join(sourceDirectory, "rootfs.squashfs")
	scratchPath := filepath.Join(sourceDirectory, "scratch.ext4")
	for _, path := range []string{kernelPath, rootfsPath, scratchPath} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source := &recordingReadOnlyDriveSource{}
	sdkMachine := &firecracker.Machine{
		Cfg: firecracker.Config{
			KernelImagePath: kernelPath,
			JailerCfg: &firecracker.JailerConfig{
				ExecFile:      "/usr/bin/firecracker",
				ChrootBaseDir: chrootBase,
				ID:            vmID,
				UID:           firecracker.Int(os.Getuid()),
				GID:           firecracker.Int(os.Getgid()),
			},
			Drives: runtimeDrivesWithComputer(
				rootfsPath,
				scratchPath,
				"",
				[]vm.ReadOnlyDrive{{
					ID:     vm.ProgramDrive,
					Source: source,
				}},
				nil,
			),
		},
		Handlers: firecracker.Handlers{
			FcInit: firecracker.HandlerList{}.Append(firecracker.Handler{
				Name: firecracker.CreateLogFilesHandlerName,
				Fn: func(context.Context, *firecracker.Machine) error {
					return nil
				},
			}),
		},
	}
	firecracker.WithLogger(logrus.NewEntry(logrus.New()))(sdkMachine)
	strategy := sealedDriveChrootStrategy{
		kernelImagePath: kernelPath,
		drives: []vm.ReadOnlyDrive{{
			ID:     vm.ProgramDrive,
			Source: source,
		}},
	}
	if err := strategy.AdaptHandlers(&sdkMachine.Handlers); err != nil {
		t.Fatal(err)
	}
	if err := sdkMachine.Handlers.FcInit.Run(context.Background(), sdkMachine); err != nil {
		t.Fatal(err)
	}
	if source.directory != root ||
		source.name != "program.squashfs" ||
		source.uid != os.Getuid() ||
		source.gid != os.Getgid() {
		t.Fatalf("link request = %+v", source)
	}
	if len(sdkMachine.Cfg.Drives) != 3 {
		t.Fatalf("drive count = %d, want 3", len(sdkMachine.Cfg.Drives))
	}
	if got := firecracker.StringValue(sdkMachine.Cfg.Drives[2].PathOnHost); got != "program.squashfs" {
		t.Fatalf("program drive path = %q", got)
	}
	for _, name := range []string{
		filepath.Base(kernelPath),
		filepath.Base(rootfsPath),
		filepath.Base(scratchPath),
	} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("ordinary jail link %q: %v", name, err)
		}
	}
}

func TestValidateReadOnlyDrivesRejectsInvalidDeclarations(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	for _, test := range []struct {
		name   string
		drives []vm.ReadOnlyDrive
	}{
		{
			name:   "path ID",
			drives: []vm.ReadOnlyDrive{{ID: "../code", Source: source}},
		},
		{
			name:   "nil source",
			drives: []vm.ReadOnlyDrive{{ID: vm.ProgramDrive}},
		},
		{
			name: "duplicate ID",
			drives: []vm.ReadOnlyDrive{
				{ID: vm.ProgramDrive, Source: source},
				{ID: vm.ProgramDrive, Source: source},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateReadOnlyDrives(test.drives); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

type fileLinkReadOnlyDriveSource struct{ path string }

func (source fileLinkReadOnlyDriveSource) LinkInto(
	directory string,
	name string,
	_ int,
	_ int,
) error {
	return os.Link(source.path, filepath.Join(directory, name))
}

type failingReadOnlyDriveSource struct{ err error }

func (source failingReadOnlyDriveSource) LinkInto(string, string, int, int) error {
	return source.err
}
