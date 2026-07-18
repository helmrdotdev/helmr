//go:build linux

package worker

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type systemStorageProbe struct{}

func (systemStorageProbe) Lstat(path string) (storageFile, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return storageFile{}, err
	}
	return storageFile{
		Kind:   linuxStorageFileKind(stat.Mode),
		Device: linuxDevice(uint64(stat.Dev)),
		RDev:   linuxDevice(uint64(stat.Rdev)),
		Size:   stat.Size,
		Blocks: stat.Blocks,
	}, nil
}

func (systemStorageProbe) StatFS(path string) (storageFS, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return storageFS{}, err
	}
	blockSize := uint64(stat.Frsize)
	if blockSize == 0 {
		blockSize = uint64(stat.Bsize)
	}
	return storageFS{
		Blocks:    stat.Blocks,
		Free:      stat.Bfree,
		Available: stat.Bavail,
		BlockSize: blockSize,
	}, nil
}

func (systemStorageProbe) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func ProveBuildStorage(config BuildStorageConfig) (BuildStorageProof, error) {
	return proveBuildStorage(config, systemStorageProbe{})
}

func linuxStorageFileKind(mode uint32) storageFileKind {
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return storageFileDirectory
	case unix.S_IFLNK:
		return storageFileSymlink
	case unix.S_IFBLK:
		return storageFileBlock
	case unix.S_IFREG:
		return storageFileRegular
	default:
		return storageFileUnknown
	}
}

func linuxDevice(device uint64) string {
	return fmt.Sprintf("%d:%d", unix.Major(device), unix.Minor(device))
}
