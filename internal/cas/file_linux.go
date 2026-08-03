//go:build linux

package cas

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type FileIdentity struct {
	device uint64
	inode  uint64
	size   int64
	mode   uint32
	uid    uint32
	gid    uint32
}

func InspectPublishedFile(file *os.File) (FileIdentity, error) {
	if file == nil {
		return FileIdentity{}, errors.New("published file is nil")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return FileIdentity{}, fmt.Errorf("stat published file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return FileIdentity{}, errors.New("published file is not regular")
	}
	if stat.Mode&0o7777 != 0o400 {
		return FileIdentity{}, fmt.Errorf("published file mode = %#o, want 0400", stat.Mode&0o7777)
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("inspect published file descriptor: %w", err)
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return FileIdentity{}, errors.New("published file descriptor is not read-only")
	}
	return FileIdentity{
		device: uint64(stat.Dev),
		inode:  stat.Ino,
		size:   stat.Size,
		mode:   stat.Mode,
		uid:    stat.Uid,
		gid:    stat.Gid,
	}, nil
}
