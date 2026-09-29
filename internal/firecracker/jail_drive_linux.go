//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/filepack"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/vm"
)

func isProgramDriveSet(drives []vm.ReadOnlyDrive) bool {
	if len(drives) != 2 {
		return false
	}
	present := make(map[string]bool, len(drives))
	for _, drive := range drives {
		switch drive.ID {
		case vm.ProgramRuntimeDrive,
			vm.ProgramDrive:
			present[drive.ID] = true
		default:
			return false
		}
	}
	return present[vm.ProgramRuntimeDrive] &&
		present[vm.ProgramDrive]
}

func runtimeDrivesWithComputer(
	rootfsPath string,
	scratchDiskPath string,
	computerDiskPath string,
	readOnlyDrives []vm.ReadOnlyDrive,
	readOnlyDrivePaths map[string]string,
) []models.Drive {
	drives := []models.Drive{{
		DriveID:      firecracker.String(rootfsDriveID),
		PathOnHost:   firecracker.String(rootfsPath),
		IsRootDevice: firecracker.Bool(true),
		IsReadOnly:   firecracker.Bool(true),
	}, {
		DriveID:      firecracker.String(scratchDriveID),
		PathOnHost:   firecracker.String(scratchDiskPath),
		IsRootDevice: firecracker.Bool(false),
		IsReadOnly:   firecracker.Bool(false),
	}}
	if strings.TrimSpace(computerDiskPath) != "" {
		drives = append(drives, models.Drive{
			DriveID:      firecracker.String(computerDriveID),
			PathOnHost:   firecracker.String(computerDiskPath),
			IsRootDevice: firecracker.Bool(false),
			IsReadOnly:   firecracker.Bool(false),
		})
	}
	byID := make(map[string]vm.ReadOnlyDrive, len(readOnlyDrives))
	for _, drive := range readOnlyDrives {
		byID[drive.ID] = drive
	}
	for _, id := range readOnlyDriveOrder {
		if _, exists := byID[id]; exists {
			pathOnHost := readOnlyDriveName(id)
			if stagedPath, staged := readOnlyDrivePaths[id]; staged {
				pathOnHost = stagedPath
			}
			drives = append(drives, models.Drive{
				DriveID:      firecracker.String(id),
				PathOnHost:   firecracker.String(pathOnHost),
				IsRootDevice: firecracker.Bool(false),
				IsReadOnly:   firecracker.Bool(true),
			})
		}
	}
	for i := range drives {
		drives[i].IoEngine = firecracker.String(blockIOEngine)
		if !*drives[i].IsReadOnly {
			drives[i].CacheType = firecracker.String(writableBlockCache)
		}
	}
	return drives
}

func prepareRestoreReadOnlyDrivePaths(
	ownerDir string,
	drives []vm.ReadOnlyDrive,
	uid int,
	gid int,
) (map[string]string, error) {
	if !filepath.IsAbs(ownerDir) {
		return nil, errors.New("restore owner directory must be absolute")
	}
	paths := make(map[string]string, len(drives))
	byID := make(map[string]vm.ReadOnlyDrive, len(drives))
	for _, drive := range drives {
		byID[drive.ID] = drive
	}
	for _, id := range readOnlyDriveOrder {
		drive, exists := byID[id]
		if !exists {
			continue
		}
		name := readOnlyDriveName(id)
		if err := drive.Source.LinkInto(ownerDir, name, uid, gid); err != nil {
			return nil, fmt.Errorf(
				"link sealed restore drive %q into owner state: %w",
				id,
				err,
			)
		}
		path := filepath.Join(ownerDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect sealed restore drive %q: %w", id, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("sealed restore drive %q is not regular", id)
		}
		paths[id] = path
	}
	return paths, nil
}

func validateReadOnlyDrives(drives []vm.ReadOnlyDrive) error {
	ids := make(map[string]struct{}, len(drives))
	for index, drive := range drives {
		switch drive.ID {
		case vm.ProgramRuntimeDrive,
			vm.ProgramDrive:
		default:
			return fmt.Errorf(
				"read-only drive %d ID %q is invalid",
				index,
				drive.ID,
			)
		}
		if drive.Source == nil {
			return fmt.Errorf("read-only drive %q source is nil", drive.ID)
		}
		if _, exists := ids[drive.ID]; exists {
			return fmt.Errorf("read-only drive ID %q is duplicated", drive.ID)
		}
		ids[drive.ID] = struct{}{}
	}
	return nil
}

func validateProgramDriveIdentities(drives []vm.ReadOnlyDrive) error {
	if !isProgramDriveSet(drives) {
		return errors.New("managed program requires the complete read-only drive set")
	}
	for _, drive := range drives {
		if _, err := cas.ObjectKey("", drive.Digest); err != nil {
			return fmt.Errorf("managed program drive %q digest: %w", drive.ID, err)
		}
		if drive.SizeBytes <= 0 {
			return fmt.Errorf("managed program drive %q size must be positive", drive.ID)
		}
		if strings.TrimSpace(drive.MediaType) == "" || drive.MediaType != strings.TrimSpace(drive.MediaType) {
			return fmt.Errorf("managed program drive %q media type must be canonical", drive.ID)
		}
	}
	return nil
}

func readOnlyDriveName(id string) string {
	return id + readOnlyDriveSuffix
}

func (c *Connector) prepareScratchDiskForJailer(scratchDiskPath string) error {
	if err := os.Chown(scratchDiskPath, c.cfg.JailerUID, c.cfg.JailerGID); err != nil {
		return fmt.Errorf("chown scratch disk for jailer: %w", err)
	}
	if err := os.Chmod(scratchDiskPath, 0o600); err != nil {
		return fmt.Errorf("chmod scratch disk for jailer: %w", err)
	}
	return nil
}

func jailRootPath(cfg Config, id string) string {
	return filepath.Join(cfg.JailerChrootBaseDir, filepath.Base(cfg.FirecrackerPath), id, "root")
}

type sealedDriveChrootStrategy struct {
	kernelImagePath string
	drives          []vm.ReadOnlyDrive
}

func (strategy sealedDriveChrootStrategy) AdaptHandlers(
	handlers *firecracker.Handlers,
) error {
	if !handlers.FcInit.Has(firecracker.CreateLogFilesHandlerName) {
		return firecracker.ErrRequiredHandlerMissing
	}
	base := firecracker.LinkFilesHandler(filepath.Base(strategy.kernelImagePath))
	base.Fn = strategy.linkFiles(base.Fn)
	handlers.FcInit = handlers.FcInit.AppendAfter(
		firecracker.CreateLogFilesHandlerName,
		base,
	)
	return nil
}

func withRestoreSealedDrives(strategy sealedDriveChrootStrategy) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		// firecracker.WithSnapshot replaces FcInit after the jailer chroot
		// strategy has adapted it. Restore the sealed-drive handler after the
		// snapshot and ordinary restore-file options have established their
		// handler list. AppendAfter places this handler before the ordinary
		// restore-file handler, so its SDK link step still sees absolute paths.
		base := firecracker.LinkFilesHandler(filepath.Base(strategy.kernelImagePath))
		base.Fn = strategy.linkFiles(base.Fn)
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(
			firecracker.CreateLogFilesHandlerName,
			base,
		)
	}
}

func (strategy sealedDriveChrootStrategy) linkFiles(
	linkOrdinary func(context.Context, *firecracker.Machine) error,
) func(context.Context, *firecracker.Machine) error {
	return func(ctx context.Context, machine *firecracker.Machine) error {
		sources := make(map[string]vm.ReadOnlyDriveSource, len(strategy.drives))
		for _, drive := range strategy.drives {
			sources[drive.ID] = drive.Source
		}
		ordinary := make([]models.Drive, 0, len(machine.Cfg.Drives))
		sealed := make(map[string]models.Drive, len(strategy.drives))
		for _, drive := range machine.Cfg.Drives {
			id := firecracker.StringValue(drive.DriveID)
			if _, exists := sources[id]; exists {
				sealed[id] = drive
				continue
			}
			ordinary = append(ordinary, drive)
		}
		machine.Cfg.Drives = ordinary
		if err := linkOrdinary(ctx, machine); err != nil {
			return err
		}

		root := jailRootPath(Config{
			FirecrackerPath:     machine.Cfg.JailerCfg.ExecFile,
			JailerChrootBaseDir: machine.Cfg.JailerCfg.ChrootBaseDir,
		}, machine.Cfg.JailerCfg.ID)
		for _, id := range readOnlyDriveOrder {
			source, exists := sources[id]
			if !exists {
				continue
			}
			drive, exists := sealed[id]
			if !exists {
				return fmt.Errorf("sealed drive %q is absent from machine config", id)
			}
			name := readOnlyDriveName(id)
			if err := source.LinkInto(
				root,
				name,
				*machine.Cfg.JailerCfg.UID,
				*machine.Cfg.JailerCfg.GID,
			); err != nil {
				return fmt.Errorf("link sealed drive %q into jail: %w", id, err)
			}
			drive.PathOnHost = firecracker.String(name)
			machine.Cfg.Drives = append(machine.Cfg.Drives, drive)
		}
		return nil
	}
}

func withJailedRestoreFiles(rootfsPath string, scratchDiskPath string, computerDiskPath string, memoryPath string, statePath string) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.Validation = machine.Handlers.Validation.Append(firecracker.JailerConfigValidationHandler)
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(firecracker.CreateLogFilesHandlerName, firecracker.Handler{
			Name: "fcinit.LinkHelmrRestoreFilesToRootFS",
			Fn: func(ctx context.Context, machine *firecracker.Machine) error {
				root := jailRootPath(Config{
					FirecrackerPath:     machine.Cfg.JailerCfg.ExecFile,
					JailerChrootBaseDir: machine.Cfg.JailerCfg.ChrootBaseDir,
				}, machine.Cfg.JailerCfg.ID)
				if err := linkIntoJail(rootfsPath, root, filepath.Base(rootfsPath)); err != nil {
					return fmt.Errorf("link rootfs into jail: %w", err)
				}
				for i := range machine.Cfg.Drives {
					if firecracker.StringValue(machine.Cfg.Drives[i].PathOnHost) == rootfsPath {
						machine.Cfg.Drives[i].PathOnHost = firecracker.String(filepath.Base(rootfsPath))
					}
				}
				if err := linkWritableDiskIntoJail(scratchDiskPath, root, scratchDiskName, *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID, false); err != nil {
					return fmt.Errorf("link scratch disk into jail: %w", err)
				}
				for i := range machine.Cfg.Drives {
					if firecracker.StringValue(machine.Cfg.Drives[i].PathOnHost) == scratchDiskPath {
						machine.Cfg.Drives[i].PathOnHost = firecracker.String(scratchDiskName)
					}
				}
				if computerDiskPath != "" {
					if err := linkWritableDiskIntoJail(computerDiskPath, root, "computer.ext4", *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID, true); err != nil {
						return fmt.Errorf("link Computer into restore jail: %w", err)
					}
					for i := range machine.Cfg.Drives {
						if firecracker.StringValue(machine.Cfg.Drives[i].PathOnHost) == computerDiskPath {
							machine.Cfg.Drives[i].PathOnHost = firecracker.String("computer.ext4")
						}
					}
				}
				if err := linkWritableDiskIntoJail(memoryPath, root, filepath.Base(memoryPath), *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID, false); err != nil {
					return fmt.Errorf("link snapshot memory into jail: %w", err)
				}
				if err := linkWritableDiskIntoJail(statePath, root, filepath.Base(statePath), *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID, false); err != nil {
					return fmt.Errorf("link snapshot state into jail: %w", err)
				}
				machine.Cfg.Snapshot.MemFilePath = path.Join("/", filepath.Base(memoryPath))
				machine.Cfg.Snapshot.SnapshotPath = path.Join("/", filepath.Base(statePath))
				return nil
			},
		})
	}
}

// Writable disks must retain one inode: capture reads the instance path while
// the VMM writes its jailed link. A clone or copy would split those histories.
func linkWritableDiskIntoJail(source, root, name string, uid, gid int, allowBlock bool) error {
	if name != filepath.Base(name) || name == "." || name == ".." {
		return errors.New("writable disk jail name must be a basename")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && (!allowBlock || !validComputerBacking(info)) {
		return errors.New("invalid writable backing type")
	}
	target := filepath.Join(root, name)
	created := false
	if err := os.Link(source, target); err != nil {
		// The SDK's sealed-drive handler may already have linked ordinary drives.
		// Accept only that exact inode; never replace an unrelated existing target.
		linked, statErr := os.Lstat(target)
		if !errors.Is(err, os.ErrExist) || statErr != nil || !os.SameFile(info, linked) {
			return fmt.Errorf("link writable disk without copying: %w", err)
		}
	} else {
		created = true
	}
	if err := chownJailFile(target, uid, gid); err != nil {
		if created {
			return errors.Join(err, os.Remove(target))
		}
		return err
	}
	return nil
}

func linkIntoJailForVMM(source string, root string, name string, uid int, gid int) error {
	if err := linkIntoJail(source, root, name); err != nil {
		return err
	}
	return chownJailFile(filepath.Join(root, name), uid, gid)
}

func linkIntoJail(source string, root string, name string) error {
	dest := filepath.Join(root, name)
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(source, dest); err == nil {
		return nil
	}
	if err := filepack.Copy(source, dest); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func chownJailFile(path string, uid int, gid int) error {
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
