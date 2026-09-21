package oci

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Filesystem is the merged image namespace. Guest metadata never becomes host
// ownership, permissions or xattrs: only regular contents live in private scratch
// files. The caller owns scratch until disk construction finishes.
type Filesystem struct {
	scratch string
	entries map[string]*imageInode
}

type imageInode struct {
	header  tar.Header
	content string
	links   int
}

// UnpackFilesystem reads the same verified OCI blobs as Unpack, retaining authored
// metadata for offline disk construction instead of materializing guest paths.
func UnpackFilesystem(r io.Reader, scratch string) (Metadata, *Filesystem, error) {
	fs := &Filesystem{scratch: scratch, entries: map[string]*imageInode{
		".": {header: tar.Header{Typeflag: tar.TypeDir, Mode: 0755, ModTime: time.Unix(0, 0)}, links: 1},
	}}
	if err := os.MkdirAll(scratch, 0700); err != nil {
		return Metadata{}, nil, err
	}
	image, err := unpack(r, scratch, scratch, fs.applyLayer)
	if err != nil {
		return Metadata{}, nil, err
	}
	return Metadata{Config: image.Config, Platform: image.Platform, ManifestCount: image.ManifestCount}, fs, nil
}

func imagePath(name string) (string, error) {
	clean := path.Clean(name)
	if strings.ContainsRune(name, 0) || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe image path %q", name)
	}
	return clean, nil
}

func sourceHeader(h *tar.Header) (tar.Header, error) {
	if h.Uid < 0 || h.Gid < 0 || uint64(h.Uid) >= 1<<32-1 || uint64(h.Gid) >= 1<<32-1 || h.Mode < 0 {
		return tar.Header{}, fmt.Errorf("invalid image ownership or mode for %q", h.Name)
	}
	// Extended attributes are retained as bytes, never installed on the host.
	// Reject other semantic extensions rather than silently normalizing them away.
	for key := range h.PAXRecords {
		switch key {
		case "path", "linkpath", "size", "uid", "gid", "uname", "gname", "mtime", "atime", "ctime":
		default:
			if !strings.HasPrefix(key, "SCHILY.xattr.") {
				return tar.Header{}, fmt.Errorf("unsupported image metadata %q on %q", key, h.Name)
			}
		}
	}
	return tar.Header{Typeflag: h.Typeflag, Mode: h.Mode & 07777, Uid: h.Uid, Gid: h.Gid, Size: h.Size, Linkname: h.Linkname, ModTime: h.ModTime, Xattrs: maps.Clone(h.Xattrs), Format: tar.FormatPAX}, nil
}

func (fs *Filesystem) parents(name string) error {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if inode, ok := fs.entries[parent]; ok {
			if inode.header.Typeflag != tar.TypeDir {
				return fmt.Errorf("image parent %q is not a directory", parent)
			}
		} else {
			fs.entries[parent] = &imageInode{header: tar.Header{Typeflag: tar.TypeDir, Mode: 0755, ModTime: time.Unix(0, 0)}, links: 1}
		}
	}
	return nil
}

func (fs *Filesystem) remove(name string) error {
	return fs.replace(name, nil)
}

func (fs *Filesystem) release(inode *imageInode) error {
	inode.links--
	if inode.links == 0 && inode.content != "" {
		return os.Remove(inode.content)
	}
	return nil
}

func (fs *Filesystem) replace(name string, replacement *imageInode) error {
	// Pin the replacement before unlinking; it may itself belong to this subtree.
	if replacement != nil {
		replacement.links++
	}
	previous := fs.entries[name]
	if previous != nil {
		if previous.header.Typeflag == tar.TypeDir {
			for key, inode := range fs.entries {
				if key == name || strings.HasPrefix(key, name+"/") {
					delete(fs.entries, key)
					if err := fs.release(inode); err != nil {
						return err
					}
				}
			}
		} else {
			delete(fs.entries, name)
			if err := fs.release(previous); err != nil {
				return err
			}
		}
	}
	if replacement != nil {
		fs.entries[name] = replacement
	}
	return nil
}

func (fs *Filesystem) applyLayer(r io.Reader, _ string) error {
	lower := maps.Clone(fs.entries)
	written := map[string]bool{}
	reader := tar.NewReader(r)
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := h.Name
		if tarEntryIsRootDir(h) {
			name = "."
		}
		name, err = imagePath(name)
		if err != nil {
			return err
		}
		if name == "." && h.Typeflag != tar.TypeDir {
			return errors.New("image root must be a directory")
		}
		base := path.Base(name)
		if strings.HasPrefix(base, ".wh.") {
			target := path.Join(path.Dir(name), strings.TrimPrefix(base, ".wh."))
			opaque := base == ".wh..wh..opq"
			if opaque {
				target = path.Dir(name)
			}
			if !opaque && (base == ".wh." || target == "." || target == ".." || path.Dir(target) != path.Dir(name)) {
				return fmt.Errorf("invalid image whiteout %q", name)
			}
			// Whiteouts affect only lower-layer names, regardless of archive ordering.
			ancestors := map[string]bool{}
			for name := range written {
				if fs.entries[name] == nil {
					continue
				}
				for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
					ancestors[parent] = true
				}
			}
			for key, inode := range lower {
				inside := key == target || strings.HasPrefix(key, target+"/") || target == "."
				if !inside || (opaque && key == target) || fs.entries[key] == nil || written[key] {
					continue
				}

				if !ancestors[key] {
					delete(fs.entries, key)
					if err := fs.release(inode); err != nil {
						return err
					}
				}
			}

			continue
		}
		written[name] = true
		header, err := sourceHeader(h)
		if err != nil {
			return err
		}
		if err := fs.parents(name); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if previous := fs.entries[name]; previous != nil && previous.header.Typeflag != tar.TypeDir {
				if err := fs.remove(name); err != nil {
					return err
				}
			}
			fs.entries[name] = &imageInode{header: header, links: 1}
		case tar.TypeReg:
			if err := fs.remove(name); err != nil {
				return err
			}
			file, err := os.CreateTemp(fs.scratch, "content-*")
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				_ = os.Remove(file.Name())
				return err
			}
			fs.entries[name] = &imageInode{header: header, content: file.Name(), links: 1}
		case tar.TypeSymlink:
			if h.Linkname == "" || strings.ContainsRune(h.Linkname, 0) {
				return fmt.Errorf("invalid image symlink %q", name)
			}
			if err := fs.remove(name); err != nil {
				return err
			}
			fs.entries[name] = &imageInode{header: header, links: 1}
		case tar.TypeLink:
			target, err := imagePath(h.Linkname)
			if err != nil {
				return err
			}
			inode := fs.entries[target]
			if inode == nil || inode.header.Typeflag != tar.TypeReg {
				return fmt.Errorf("image hardlink %q requires an existing regular target", name)
			}
			if name == target {
				continue
			}
			if err := fs.replace(name, inode); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported image entry %q type %d", name, h.Typeflag)
		}
	}
}

// LogicalBytes measures regular inode contents and symlink targets once. Disk
// metadata and filesystem free-space allowance remain the generator's concern.
func (fs *Filesystem) LogicalBytes() (int64, error) {
	var total int64
	seen := map[*imageInode]bool{}
	for _, inode := range fs.entries {
		if seen[inode] {
			continue
		}
		seen[inode] = true
		size := inode.header.Size
		if inode.header.Typeflag == tar.TypeSymlink {
			size = int64(len(inode.header.Linkname))
		}
		if size < 0 || size > int64(^uint64(0)>>1)-total {
			return 0, errors.New("image size overflow")
		}
		total += size
	}
	return total, nil
}

// WriteArchive emits only source metadata, in deterministic namespace order.
// Hardlink contents are emitted at the first surviving name, even if the original
// target was removed by a later layer. Scratch ownership and xattrs are irrelevant.
func (fs *Filesystem) WriteArchive(output io.Writer) error {
	names := make([]string, 0, len(fs.entries))
	for name := range fs.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	writer := tar.NewWriter(output)
	seen := map[*imageInode]string{}
	for _, name := range names {
		inode := fs.entries[name]
		header := inode.header
		header.Name = name
		if header.Typeflag == tar.TypeDir {
			header.Name += "/"
		}
		if first, ok := seen[inode]; ok {
			header.Typeflag = tar.TypeLink
			header.Linkname = first
			header.Size = 0
		} else {
			seen[inode] = name
		}
		if err := writer.WriteHeader(&header); err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		file, err := os.Open(inode.content)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
	}
	return writer.Close()
}
