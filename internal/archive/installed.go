package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

func appendInstalledTree(
	ctx context.Context,
	writer *tar.Writer,
	root string,
	excludeMatchers []*regexp.Regexp,
	options TarOptions,
	stats *tarStats,
) error {
	type pendingEntry struct {
		name     string
		sortKey  string
		info     os.FileInfo
		linkname string
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer confined.Close()
	hostRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	hostRoot, err = filepath.EvalSymlinks(hostRoot)
	if err != nil {
		return err
	}
	var entries []pendingEntry
	// The encoder supplies the root directory; reserve its one-byte name.
	stats.names = 1
	var collect func(string) error
	collect = func(parent string) error {
		directory, err := confined.Open(parent)
		if err != nil {
			return err
		}
		defer directory.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			children, readErr := directory.ReadDir(128)
			for _, child := range children {
				rel := child.Name()
				if parent != "." {
					rel = parent + "/" + rel
				}
				if excludedRelativePath(rel, excludeMatchers) {
					continue
				}
				if options.MaxEntries > 0 && len(entries) >= options.MaxEntries {
					return errors.New("tar archive contains too many entries")
				}
				if err := safepath.ValidateTreePath(rel, "/workspace/project", "/workspace/program", "/opt/helmr/program"); err != nil {
					return fmt.Errorf("installed path %q: %w", rel, err)
				}
				if err := safepath.ValidateHostTreePath(hostRoot, rel); err != nil {
					return err
				}
				info, err := confined.Lstat(rel)
				if err != nil {
					return err
				}
				linkname := ""
				if info.Mode()&os.ModeSymlink != 0 {
					linkname, err = confined.Readlink(rel)
					if err != nil {
						return err
					}
					if err := safepath.ValidateTreeLink(linkname); err != nil {
						return err
					}
				}
				charge := int64(len(rel)) + int64(len(linkname))
				if options.MaxNameBytes > 0 && charge > options.MaxNameBytes-stats.names {
					return errors.New("tar archive exceeds name budget")
				}
				stats.names += charge
				if info.Mode().IsRegular() {
					if options.MaxFileBytes > 0 && info.Size() > options.MaxFileBytes {
						return fmt.Errorf("tar entry %q exceeds file size limit", rel)
					}
					if err := validateAppendSize(rel, info.Size(), options.MaxBytes, stats); err != nil {
						return err
					}
				}
				sortKey := rel
				if info.IsDir() {
					sortKey += "/"
				}
				entries = append(entries, pendingEntry{name: rel, sortKey: sortKey, info: info, linkname: linkname})
				if info.IsDir() {
					if err := collect(rel); err != nil {
						return err
					}
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	if err := collect("."); err != nil {
		return err
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].sortKey < entries[right].sortKey
	})
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info := entry.info
		linkname := entry.linkname
		stats.entries++
		header, err := tar.FileInfoHeader(info, linkname)
		if err != nil {
			return err
		}
		normalizeHeader(header, entry.name, true)
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		var content io.Writer = writer
		if options.ObserveEntry != nil {
			observer, err := options.ObserveEntry(header)
			if err != nil {
				return err
			}
			if observer != nil {
				content = io.MultiWriter(writer, observer)
			}
		}
		if !info.Mode().IsRegular() {
			continue
		}
		file, err := confined.Open(entry.name)
		if err != nil {
			return err
		}
		before, err := file.Stat()
		if err != nil || !os.SameFile(info, before) || info.Mode() != before.Mode() || info.Size() != before.Size() || !info.ModTime().Equal(before.ModTime()) {
			_ = file.Close()
			return fmt.Errorf("tar entry %q changed during capture", entry.name)
		}
		_, copyErr := io.Copy(content, contextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
