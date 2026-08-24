package substrate

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultExtraBytes = int64(128 * 1024 * 1024)
	ext4Features      = "sparse_super,large_file,filetype,resize_inode,dir_index,ext_attr,has_journal,extent,huge_file,flex_bg,metadata_csum,metadata_csum_seed,64bit,dir_nlink,extra_isize,orphan_file"
)

func substrateDiskSize(rootfsDir string) (int64, error) {
	var total int64
	if err := filepath.WalkDir(rootfsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular():
			total += info.Size()
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			total += int64(len(target))
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("measure substrate rootfs: %w", err)
	}
	size := total + defaultExtraBytes
	const minSize = int64(256 * 1024 * 1024)
	if size < minSize {
		size = minSize
	}
	const block = int64(4 * 1024 * 1024)
	if rem := size % block; rem != 0 {
		size += block - rem
	}
	return size, nil
}

func createExt4(
	ctx context.Context,
	mkfs string,
	mke2fsConfig string,
	rootfsDir string,
	path string,
	sizeBytes int64,
	key string,
) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create substrate ext4: %w", err)
	}
	if err := file.Truncate(sizeBytes); err != nil {
		_ = file.Close()
		return fmt.Errorf("size substrate ext4: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close substrate ext4: %w", err)
	}
	uuid := deterministicUUID(key)
	cmd := exec.CommandContext(
		ctx,
		mkfs,
		"-F",
		"-q",
		"-t", "ext4",
		"-b", "4096",
		"-I", "256",
		"-i", "16384",
		"-m", "0",
		"-O", ext4Features,
		"-U", uuid,
		"-E", "hash_seed="+uuid+",lazy_itable_init=0,lazy_journal_init=0,nodiscard,root_owner=0:0",
		"-d", "-",
		path,
	)
	cmd.Env = []string{
		"LC_ALL=C.UTF-8",
		"LANG=C.UTF-8",
		"TZ=UTC",
		"SOURCE_DATE_EPOCH=0",
		"MKE2FS_CONFIG=" + mke2fsConfig,
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open mkfs substrate input: %w", err)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mkfs substrate ext4: %w", err)
	}
	archiveErr := writeCanonicalRootfsArchive(rootfsDir, stdin)
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("mkfs substrate ext4: %w: %s", waitErr, strings.TrimSpace(output.String()))
	}
	if archiveErr != nil {
		return archiveErr
	}
	if closeErr != nil {
		return fmt.Errorf("close mkfs substrate input: %w", closeErr)
	}
	return nil
}

func writeCanonicalRootfsArchive(rootfsDir string, output io.Writer) error {
	writer := tar.NewWriter(output)
	seenHardlinks := map[fileIdentity]string{}
	walkErr := filepath.WalkDir(rootfsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == rootfsDir {
			return nil
		}
		relative, err := filepath.Rel(rootfsDir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &tar.Header{
			Name:       name,
			Mode:       int64(info.Mode().Perm()),
			Uid:        0,
			Gid:        0,
			ModTime:    time.Unix(0, 0).UTC(),
			AccessTime: time.Time{},
			ChangeTime: time.Time{},
			Format:     tar.FormatPAX,
		}
		switch {
		case info.IsDir():
			header.Typeflag = tar.TypeDir
			header.Name += "/"
		case info.Mode().IsRegular():
			identity, links, err := regularFileIdentity(info)
			if err != nil {
				return err
			}
			if links > 1 {
				if target, ok := seenHardlinks[identity]; ok {
					header.Typeflag = tar.TypeLink
					header.Linkname = target
					break
				}
				seenHardlinks[identity] = name
			}
			header.Typeflag = tar.TypeReg
			header.Size = info.Size()
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Typeflag = tar.TypeSymlink
			header.Linkname = target
		default:
			return fmt.Errorf("unsupported substrate rootfs entry %q mode %s", name, info.Mode())
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeWriterErr := writer.Close()
	if walkErr != nil {
		return fmt.Errorf("encode canonical substrate rootfs archive: %w", walkErr)
	}
	if closeWriterErr != nil {
		return fmt.Errorf("close canonical substrate rootfs archive: %w", closeWriterErr)
	}
	return nil
}

type fileIdentity struct {
	device uint64
	inode  uint64
}

func regularFileIdentity(info os.FileInfo) (fileIdentity, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, 0, errors.New("inspect substrate rootfs hardlink identity: unsupported stat result")
	}
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, uint64(stat.Nlink), nil
}

func deterministicUUID(key string) string {
	sum := sha256.Sum256([]byte(key))
	raw := sum[:16]
	raw[6] = (raw[6] & 0x0f) | 0x50
	raw[8] = (raw[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(raw)
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32]
}
