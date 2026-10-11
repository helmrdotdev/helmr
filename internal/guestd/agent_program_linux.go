//go:build linux

package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"golang.org/x/sys/unix"
)

type mountedProgramImage struct {
	root, directory string
	snapshot        *snapshot.Artifact
	loop            *os.File
	mounted         bool
	cleanupErr      error
}

func materializeAgentProgram(ctx context.Context, root string, kind agentv1.SessionProgramRequest_Kind, descriptor *agentv1.SessionProgramArtifact, input agentProgramInput, loopMu *sync.Mutex) (result *mountedProgramImage, resultErr error) {
	if root == "" {
		root = "/var/lib/helmr/session-programs"
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(root, "image-")
	if err != nil {
		return nil, err
	}
	image := &mountedProgramImage{directory: directory, root: filepath.Join(directory, "mount")}
	defer func() {
		if resultErr != nil {
			if err := image.close(); err != nil {
				result = image
				resultErr = errors.Join(resultErr, programCleanupError{err})
			}
		}
	}()
	role := artifact.RoleProgram
	if kind == agentv1.SessionProgramRequest_KIND_RUNTIME {
		role = artifact.RoleRuntime
	}
	image.snapshot, err = snapshot.Produce(ctx, directory, role, snapshot.Owner{UID: os.Geteuid(), GID: os.Getegid()}, false, func(file *os.File) error {
		// Reserve the actual bytes before receiving them. This shares the Computer's
		// already-reserved scratch capacity and never evicts a peer's executable.
		if err := unix.Fallocate(int(file.Fd()), 0, 0, descriptor.SizeBytes); err != nil {
			return fmt.Errorf("reserve Session Program scratch: %w", err)
		}
		return input.copyArtifact(ctx, kind, descriptor, io.NewOffsetWriter(file, 0))
	})
	if err != nil {
		return nil, err
	}
	actual := image.snapshot.Descriptor()
	if actual.Digest != descriptor.Digest || actual.SizeBytes != descriptor.SizeBytes {
		return nil, errors.New("session Program bytes do not match pinned descriptor")
	}
	file, err := image.snapshot.VerifierFile()
	if err != nil {
		return nil, err
	}
	loopMu.Lock()
	err = configureProgramLoop(image, file)
	loopMu.Unlock()
	if err != nil {
		return nil, err
	}
	if err = os.Mkdir(image.root, 0700); err != nil {
		return nil, err
	}
	if err = unix.Mount(filepath.Join(directory, "device"), image.root, "squashfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return nil, err
	}
	image.mounted = true
	return image, nil
}

func (image *mountedProgramImage) close() error {
	if image.cleanupErr != nil {
		return image.cleanupErr
	}
	if image.mounted {
		if err := unix.Unmount(image.root, 0); err != nil {
			return err
		}
		image.mounted = false
	}
	if image.loop != nil {
		image.cleanupErr = errors.Join(image.cleanupErr, image.loop.Close())
		image.loop = nil
	}
	if image.snapshot != nil {
		image.cleanupErr = errors.Join(image.cleanupErr, image.snapshot.Close())
		image.snapshot = nil
	}
	image.cleanupErr = errors.Join(image.cleanupErr, os.RemoveAll(image.directory))
	return image.cleanupErr
}

// A store serializes allocation so its concurrent Sessions cannot claim the
// same reported free loop index. EBUSY from an external owner still fails safely.
func configureProgramLoop(image *mountedProgramImage, file *os.File) error {
	control, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	number, loopErr := unix.IoctlRetInt(int(control.Fd()), unix.LOOP_CTL_GET_FREE)
	closeErr := control.Close()
	if loopErr != nil || closeErr != nil {
		return errors.Join(loopErr, closeErr)
	}
	node := filepath.Join(image.directory, "device")
	if err = unix.Mknod(node, unix.S_IFBLK|0600, int(unix.Mkdev(7, uint32(number)))); err != nil {
		return err
	}
	image.loop, err = os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	// An EBUSY loser never reconfigures or clears another owner's loop device.
	if err = unix.IoctlLoopConfigure(int(image.loop.Fd()), &unix.LoopConfig{Fd: uint32(file.Fd()), Info: unix.LoopInfo64{Flags: unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR}}); err != nil {
		return err
	}

	return nil
}
