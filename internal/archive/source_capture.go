package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const SourceMediaType = "application/vnd.helmr.deployment-source.v0+tar"

type sourceSnapshot struct {
	root        *os.Root
	entries     []sourceEntry
	observed    map[string]sourceObserved
	directories map[string][]string
	ignore      SourceIgnore
	ignoreBody  []byte
}

type sourceEntry struct {
	name     string
	sortKey  string
	info     os.FileInfo
	linkname string
	body     []byte
}

type sourceObserved struct {
	info       os.FileInfo
	changeTime time.Time
	hasChange  bool
	linkname   string
}

func appendCanonicalSource(
	ctx context.Context,
	writer *tar.Writer,
	rootPath string,
	options TarOptions,
	stats *tarStats,
) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open canonical source root: %w", err)
	}
	defer root.Close()
	snapshot := &sourceSnapshot{
		root:        root,
		observed:    make(map[string]sourceObserved),
		directories: make(map[string][]string),
	}
	if err := snapshot.collect(ctx); err != nil {
		return err
	}
	sort.Slice(snapshot.entries, func(left, right int) bool {
		return snapshot.entries[left].sortKey < snapshot.entries[right].sortKey
	})
	for _, entry := range snapshot.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		stats.entries++
		if options.MaxEntries > 0 && stats.entries > options.MaxEntries {
			return errors.New("tar archive contains too many entries")
		}
		if entry.info.Mode().IsRegular() {
			if err := validateAppendSize(entry.name, entry.info.Size(), options.MaxBytes, stats); err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(entry.info, entry.linkname)
		if err != nil {
			return fmt.Errorf("encode canonical source entry %q: %w", entry.name, err)
		}
		normalizeHeader(header, entry.name, true)
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write canonical source header %q: %w", entry.name, err)
		}
		if !entry.info.Mode().IsRegular() {
			continue
		}
		if entry.body != nil {
			if _, err := io.Copy(writer, bytes.NewReader(entry.body)); err != nil {
				return fmt.Errorf("write canonical source file %q: %w", entry.name, err)
			}
			continue
		}
		if err := snapshot.writeFile(ctx, writer, entry); err != nil {
			return err
		}
	}
	if err := snapshot.verify(); err != nil {
		return err
	}
	return nil
}

func (snapshot *sourceSnapshot) collect(ctx context.Context) error {
	rootInfo, err := snapshot.root.Lstat(".")
	if err != nil {
		return fmt.Errorf("inspect canonical source root: %w", err)
	}
	if _, ok := sourceChangeTime(rootInfo); !ok {
		return errors.New("canonical source capture is unsupported on this platform")
	}
	snapshot.observe(".", rootInfo, "")
	ignoreInfo, ignoreBody, ignore, err := readSourceIgnore(snapshot.root)
	if err != nil {
		return err
	}
	snapshot.ignore = ignore
	snapshot.ignoreBody = ignoreBody
	if ignoreInfo != nil {
		snapshot.observe(".helmrignore", ignoreInfo, "")
	}
	return snapshot.collectDirectory(ctx, ".", rootInfo)
}

func readSourceIgnore(root *os.Root) (os.FileInfo, []byte, SourceIgnore, error) {
	info, err := root.Lstat(".helmrignore")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, SourceIgnore{}, nil
	}
	if err != nil {
		return nil, nil, SourceIgnore{}, fmt.Errorf("inspect .helmrignore: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxSourceIgnoreBytes {
		return nil, nil, SourceIgnore{}, errors.New(".helmrignore must be a regular UTF-8 file no larger than 1 MiB")
	}
	file, err := root.Open(".helmrignore")
	if err != nil {
		return nil, nil, SourceIgnore{}, fmt.Errorf("open .helmrignore: %w", err)
	}
	before, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, SourceIgnore{}, fmt.Errorf("inspect open .helmrignore: %w", err)
	}
	if !sameSourceInfo(info, before) {
		_ = file.Close()
		return nil, nil, SourceIgnore{}, sourceChanged(".helmrignore")
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxSourceIgnoreBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil {
		return nil, nil, SourceIgnore{}, fmt.Errorf("read .helmrignore: %w", readErr)
	}
	if statErr != nil {
		return nil, nil, SourceIgnore{}, fmt.Errorf("reinspect open .helmrignore: %w", statErr)
	}
	if closeErr != nil {
		return nil, nil, SourceIgnore{}, fmt.Errorf("close .helmrignore: %w", closeErr)
	}
	final, err := root.Lstat(".helmrignore")
	if err != nil || !sameSourceInfo(info, after) || !sameSourceInfo(info, final) ||
		int64(len(body)) != info.Size() {
		return nil, nil, SourceIgnore{}, sourceChanged(".helmrignore")
	}
	if len(body) > maxSourceIgnoreBytes || !utf8.Valid(body) {
		return nil, nil, SourceIgnore{}, errors.New(".helmrignore must be a regular UTF-8 file no larger than 1 MiB")
	}
	ignore, err := ParseSourceIgnore(body)
	if err != nil {
		return nil, nil, SourceIgnore{}, err
	}
	return info, body, ignore, nil
}

func (snapshot *sourceSnapshot) collectDirectory(
	ctx context.Context,
	name string,
	initial os.FileInfo,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := snapshot.root.Open(rootRelative(name))
	if err != nil {
		return sourceChanged(name)
	}
	before, err := directory.Stat()
	if err != nil || !sameSourceInfo(initial, before) || !before.IsDir() {
		_ = directory.Close()
		return sourceChanged(name)
	}
	children, readErr := directory.ReadDir(-1)
	after, statErr := directory.Stat()
	closeErr := directory.Close()
	if readErr != nil {
		return fmt.Errorf("read canonical source directory %q: %w", name, readErr)
	}
	if statErr != nil || !sameSourceInfo(initial, after) {
		return sourceChanged(name)
	}
	if closeErr != nil {
		return fmt.Errorf("close canonical source directory %q: %w", name, closeErr)
	}
	names := make([]string, 0, len(children))
	for _, child := range children {
		names = append(names, child.Name())
	}
	sort.Strings(names)
	snapshot.directories[name] = names
	for _, base := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		childName := base
		if name != "." {
			childName = name + "/" + base
		}
		if err := validateSourcePath(childName); err != nil {
			return err
		}
		info, err := snapshot.root.Lstat(rootRelative(childName))
		if err != nil {
			return sourceChanged(childName)
		}
		linkname := ""
		if info.Mode()&os.ModeSymlink != 0 {
			linkname, err = snapshot.root.Readlink(rootRelative(childName))
			if err != nil {
				return sourceChanged(childName)
			}
		}
		if childName == ".helmrignore" {
			observed, exists := snapshot.observed[childName]
			if !exists || !sameSourceInfo(observed.info, info) {
				return sourceChanged(childName)
			}
		} else {
			snapshot.observe(childName, info, linkname)
		}
		if childName == ".git" {
			continue
		}
		isDir := info.IsDir()
		if snapshot.ignore.Match(childName, isDir) && childName != ".helmrignore" {
			continue
		}
		if childName == "node_modules" || childName == "helmr" {
			return fmt.Errorf(
				"canonical source root %q is reserved; exclude it with .helmrignore",
				childName,
			)
		}
		if (info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) &&
			IsSourceSecretPath(childName) {
			return fmt.Errorf(
				"canonical source contains likely secret %q; exclude it with .helmrignore",
				childName,
			)
		}
		if err := validateSourceEntry(childName, info, linkname); err != nil {
			return err
		}
		entry := sourceEntry{
			name:     childName,
			sortKey:  childName,
			info:     info,
			linkname: linkname,
		}
		if isDir {
			entry.name += "/"
			entry.sortKey += "/"
		}
		if childName == ".helmrignore" {
			entry.body = snapshot.ignoreBody
		}
		snapshot.entries = append(snapshot.entries, entry)
		if isDir {
			if err := snapshot.collectDirectory(ctx, childName, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func (snapshot *sourceSnapshot) observe(name string, info os.FileInfo, linkname string) {
	changeTime, hasChange := sourceChangeTime(info)
	snapshot.observed[name] = sourceObserved{
		info:       info,
		changeTime: changeTime,
		hasChange:  hasChange,
		linkname:   linkname,
	}
}

func (snapshot *sourceSnapshot) writeFile(
	ctx context.Context,
	writer *tar.Writer,
	entry sourceEntry,
) error {
	file, err := snapshot.root.Open(rootRelative(entry.name))
	if err != nil {
		return sourceChanged(entry.name)
	}
	before, err := file.Stat()
	if err != nil || !sameSourceInfo(entry.info, before) || !before.Mode().IsRegular() {
		_ = file.Close()
		return sourceChanged(entry.name)
	}
	_, copyErr := io.CopyN(writer, contextReader{ctx: ctx, reader: file}, entry.info.Size())
	after, statErr := file.Stat()
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("read canonical source file %q: %w", entry.name, copyErr)
	}
	if statErr != nil || !sameSourceInfo(entry.info, after) {
		return sourceChanged(entry.name)
	}
	if closeErr != nil {
		return fmt.Errorf("close canonical source file %q: %w", entry.name, closeErr)
	}
	return nil
}

func (snapshot *sourceSnapshot) verify() error {
	for name, expected := range snapshot.directories {
		observed, exists := snapshot.observed[name]
		if !exists {
			return sourceChanged(name)
		}
		directory, err := snapshot.root.Open(rootRelative(name))
		if err != nil {
			return sourceChanged(name)
		}
		before, statErr := directory.Stat()
		if statErr != nil || !sameObservedInfo(observed, before) {
			_ = directory.Close()
			return sourceChanged(name)
		}
		children, readErr := directory.ReadDir(-1)
		after, statErr := directory.Stat()
		closeErr := directory.Close()
		if readErr != nil {
			return fmt.Errorf("recheck canonical source directory %q: %w", name, readErr)
		}
		if statErr != nil || !sameObservedInfo(observed, after) {
			return sourceChanged(name)
		}
		if closeErr != nil {
			return fmt.Errorf("close canonical source directory %q: %w", name, closeErr)
		}
		actual := make([]string, 0, len(children))
		for _, child := range children {
			actual = append(actual, child.Name())
		}
		sort.Strings(actual)
		if !slices.Equal(expected, actual) {
			return sourceChanged(name)
		}
	}
	for name, observed := range snapshot.observed {
		info, err := snapshot.root.Lstat(rootRelative(name))
		if err != nil || !sameObservedInfo(observed, info) {
			return sourceChanged(name)
		}
		if observed.info.Mode()&os.ModeSymlink != 0 {
			linkname, err := snapshot.root.Readlink(rootRelative(name))
			if err != nil || linkname != observed.linkname {
				return sourceChanged(name)
			}
		}
	}
	return nil
}

func sameSourceInfo(before, after os.FileInfo) bool {
	if before == nil || after == nil {
		return false
	}
	beforeChange, beforeHasChange := sourceChangeTime(before)
	afterChange, afterHasChange := sourceChangeTime(after)
	return os.SameFile(before, after) &&
		before.Mode() == after.Mode() &&
		before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime()) &&
		beforeHasChange == afterHasChange &&
		(!beforeHasChange || beforeChange.Equal(afterChange))
}

func sameObservedInfo(observed sourceObserved, after os.FileInfo) bool {
	if !sameSourceInfo(observed.info, after) {
		return false
	}
	changeTime, hasChange := sourceChangeTime(after)
	return observed.hasChange == hasChange &&
		(!observed.hasChange || observed.changeTime.Equal(changeTime))
}

func sourceChanged(name string) error {
	return fmt.Errorf("%w: %q", errSourceChanged, name)
}

func rootRelative(name string) string {
	if name == "." {
		return "."
	}
	return filepath.FromSlash(strings.TrimSuffix(name, "/"))
}

func validateSourcePath(name string) error {
	if !utf8.ValidString(name) || name == "" || path.IsAbs(name) || path.Clean(name) != name {
		return fmt.Errorf("canonical source contains invalid path %q", name)
	}
	for component := range strings.SplitSeq(name, "/") {
		if component == "" || component == "." || component == ".." || strings.ContainsRune(component, '\x00') {
			return fmt.Errorf("canonical source contains invalid path %q", name)
		}
	}
	return nil
}

func validateSourceEntry(name string, info os.FileInfo, linkname string) error {
	emittedName := name
	if info.IsDir() {
		emittedName += "/"
	}
	if !sourceUSTARPathRepresentable(emittedName) {
		return fmt.Errorf("canonical source path %q is not USTAR-representable", name)
	}
	switch {
	case info.Mode().IsRegular(), info.IsDir():
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		if !utf8.ValidString(linkname) || strings.ContainsRune(linkname, '\x00') ||
			linkname == "" || path.IsAbs(linkname) || len(linkname) > 100 {
			return fmt.Errorf("canonical source symlink %q has invalid target", name)
		}
		resolved := path.Clean(path.Join(path.Dir(name), linkname))
		if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) {
			return fmt.Errorf("canonical source symlink %q escapes the project root", name)
		}
		return nil
	default:
		return fmt.Errorf("canonical source entry %q has unsupported type %s", name, info.Mode().Type())
	}
}

func sourceUSTARPathRepresentable(name string) bool {
	isDir := strings.HasSuffix(name, "/")
	trimmed := strings.TrimSuffix(name, "/")
	index := strings.LastIndexByte(trimmed, '/')
	leaf := trimmed
	parent := ""
	if index >= 0 {
		parent = trimmed[:index]
		leaf = trimmed[index+1:]
	}
	if isDir {
		leaf += "/"
	}
	return leaf != "" && len(leaf) <= 100 && len(parent) <= 155
}
