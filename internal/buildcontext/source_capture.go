package buildcontext

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

const (
	maxSourceBytes      int64 = 512 << 20
	maxSourceEntries          = 100000
	maxObservationBytes int64 = 128 << 20
)

type sourceSnapshot struct {
	root          *os.Root
	entries       []sourceEntry
	observed      map[string]sourceObserved
	directories   map[string][]string
	ignore        *gitIgnore
	ignoreBody    []byte
	metadataBytes int64
	payloadBytes  int64
	metadataLimit int64
	entryLimit    int
	payloadLimit  int64
	prefixes      []string
}

type sourceEntry struct {
	name     string
	info     os.FileInfo
	linkname string
	body     []byte
}

type sourceObserved struct {
	info     os.FileInfo
	linkname string
}

func (snapshot *sourceSnapshot) collect(ctx context.Context) error {
	rootInfo, err := snapshot.root.Lstat(".")
	if err != nil {
		return fmt.Errorf("inspect build source root: %w", err)
	}
	if _, ok := sourceChangeTime(rootInfo); !ok {
		return errors.New("build source capture is unsupported on this platform")
	}
	if snapshot.metadataLimit == 0 {
		snapshot.metadataLimit = maxObservationBytes
	}
	if snapshot.entryLimit == 0 {
		snapshot.entryLimit = maxSourceEntries
	}
	if snapshot.payloadLimit == 0 {
		snapshot.payloadLimit = maxSourceBytes
	}
	if err := snapshot.observe(".", rootInfo, ""); err != nil {
		return err
	}
	ignoreInfo, ignoreBody, ignore, err := readSourceIgnore(snapshot.root)
	if err != nil {
		return err
	}
	snapshot.ignore = ignore
	snapshot.ignoreBody = ignoreBody
	if ignoreInfo != nil {
		if err := snapshot.charge(int64(len(ignoreBody))); err != nil {
			return err
		}
		if err := snapshot.observe(".helmrignore", ignoreInfo, ""); err != nil {
			return err
		}
	}
	return snapshot.collectDirectory(ctx, ".", rootInfo)
}

func readSourceIgnore(root *os.Root) (os.FileInfo, []byte, *gitIgnore, error) {
	info, err := root.Lstat(".helmrignore")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inspect .helmrignore: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxSourceIgnoreBytes {
		return nil, nil, nil, errors.New(".helmrignore must be a regular UTF-8 file no larger than 1 MiB")
	}
	file, err := root.OpenFile(".helmrignore", os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open .helmrignore: %w", err)
	}
	before, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, nil, fmt.Errorf("inspect open .helmrignore: %w", err)
	}
	if !sameSourceInfo(info, before) {
		_ = file.Close()
		return nil, nil, nil, sourceChanged(".helmrignore")
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxSourceIgnoreBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil {
		return nil, nil, nil, fmt.Errorf("read .helmrignore: %w", readErr)
	}
	if statErr != nil {
		return nil, nil, nil, fmt.Errorf("reinspect open .helmrignore: %w", statErr)
	}
	if closeErr != nil {
		return nil, nil, nil, fmt.Errorf("close .helmrignore: %w", closeErr)
	}
	final, err := root.Lstat(".helmrignore")
	if err != nil || !sameSourceInfo(info, after) || !sameSourceInfo(info, final) ||
		int64(len(body)) != info.Size() {
		return nil, nil, nil, sourceChanged(".helmrignore")
	}
	if len(body) > maxSourceIgnoreBytes || !utf8.Valid(body) {
		return nil, nil, nil, errors.New(".helmrignore must be a regular UTF-8 file no larger than 1 MiB")
	}
	ignore, err := parseSourceIgnore(body)
	if err != nil {
		return nil, nil, nil, err
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
	directory, err := snapshot.root.OpenFile(rootRelative(name), os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return sourceChanged(name)
	}
	before, err := directory.Stat()
	if err != nil || !sameSourceInfo(initial, before) || !before.IsDir() {
		_ = directory.Close()
		return sourceChanged(name)
	}
	names, readErr := snapshot.readNames(ctx, directory, name)
	after, statErr := directory.Stat()
	closeErr := directory.Close()
	if readErr != nil {
		return readErr
	}
	if statErr != nil || !sameSourceInfo(initial, after) {
		return sourceChanged(name)
	}
	if closeErr != nil {
		return closeErr
	}
	sort.Strings(names)
	if name == "." {
		if _, observed := snapshot.observed[".helmrignore"]; observed {
			if _, exact := sort.Find(len(names), func(i int) int { return strings.Compare(".helmrignore", names[i]) }); !exact {
				return errors.New("source filesystem aliases .helmrignore; exact spelling is required")
			}
		}
	}
	snapshot.directories[name] = names
	for _, base := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		childName := base
		if name != "." {
			childName = name + "/" + base
		}
		if err := validateObservedPath(childName); err != nil {
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
			if err := snapshot.observe(childName, info, linkname); err != nil {
				return err
			}
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
				"build source root %q is reserved; exclude it with .helmrignore",
				childName,
			)
		}
		if (info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) &&
			isSourceSecretPath(childName) {
			return fmt.Errorf(
				"build source contains likely secret %q; exclude it with .helmrignore",
				childName,
			)
		}
		if err := validateSourcePath(childName); err != nil {
			return err
		}
		for _, prefix := range snapshot.prefixes {
			if err := safepath.ValidateHostTreePath(prefix, childName); err != nil {
				return err
			}
		}
		if err := validateSourceEntry(childName, info, linkname); err != nil {
			return err
		}
		entry := sourceEntry{
			name:     childName,
			info:     info,
			linkname: linkname,
		}
		if childName == ".helmrignore" {
			entry.body = snapshot.ignoreBody
		}
		if len(snapshot.entries) >= snapshot.entryLimit {
			return errors.New("build source contains too many entries")
		}
		if info.Mode().IsRegular() {
			if info.Size() < 0 || info.Size() > snapshot.payloadLimit-snapshot.payloadBytes {
				return errors.New("build source exceeds logical size limit")
			}
			snapshot.payloadBytes += info.Size()
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

func (snapshot *sourceSnapshot) observe(name string, info os.FileInfo, linkname string) error {
	// Charge map/entry bookkeeping conservatively as well as retained strings.
	if err := snapshot.charge(512 + 4*int64(len(name)) + 2*int64(len(linkname))); err != nil {
		return err
	}
	snapshot.observed[name] = sourceObserved{
		info:     info,
		linkname: linkname,
	}
	return nil
}

func (snapshot *sourceSnapshot) writeFile(
	ctx context.Context,
	writer io.Writer,
	entry sourceEntry,
) error {
	file, err := snapshot.root.OpenFile(rootRelative(entry.name), os.O_RDONLY|nonblockFlag, 0)
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
		return fmt.Errorf("read build source file %q: %w", entry.name, copyErr)
	}
	if statErr != nil || !sameSourceInfo(entry.info, after) {
		return sourceChanged(entry.name)
	}
	if closeErr != nil {
		return fmt.Errorf("close build source file %q: %w", entry.name, closeErr)
	}
	return nil
}

func (snapshot *sourceSnapshot) verify(ctx context.Context) error {
	for name, expected := range snapshot.directories {
		observed, exists := snapshot.observed[name]
		if !exists {
			return sourceChanged(name)
		}
		directory, err := snapshot.root.OpenFile(rootRelative(name), os.O_RDONLY|nonblockFlag, 0)
		if err != nil {
			return sourceChanged(name)
		}
		before, statErr := directory.Stat()
		if statErr != nil || !sameSourceInfo(observed.info, before) {
			_ = directory.Close()
			return sourceChanged(name)
		}
		count := 0
		var readErr error
		for {
			if readErr = ctx.Err(); readErr != nil {
				break
			}
			children, err := directory.ReadDir(128)
			for _, child := range children {
				if _, found := sort.Find(len(expected), func(i int) int { return strings.Compare(child.Name(), expected[i]) }); !found {
					readErr = sourceChanged(name)
					break
				}
				count++
			}
			if readErr != nil || err == io.EOF {
				break
			}
			if err != nil {
				readErr = err
				break
			}
		}
		after, statErr := directory.Stat()
		closeErr := directory.Close()
		if readErr != nil {
			return readErr
		}
		if count != len(expected) || statErr != nil || !sameSourceInfo(observed.info, after) {
			return sourceChanged(name)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for name, observed := range snapshot.observed {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := snapshot.root.Lstat(rootRelative(name))
		if err != nil || !sameSourceInfo(observed.info, info) {
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
	if name == "." {
		return errors.New("invalid source entry name")
	}
	return safepath.ValidateTreePath(name, "/workspace/project", "/workspace/program", "/opt/helmr/program")
}

func validateSourceEntry(name string, info os.FileInfo, linkname string) error {
	switch {
	case info.Mode().IsRegular(), info.IsDir():
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		if err := safepath.ValidateTreeLink(linkname); err != nil {
			return fmt.Errorf("build source symlink %q: %w", name, err)
		}
		resolved := path.Clean(path.Join(path.Dir(name), linkname))
		if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) {
			return fmt.Errorf("build source symlink %q escapes the project root", name)
		}
		return safepath.ValidateTreePath(resolved, "/workspace/project", "/workspace/program", "/opt/helmr/program")
	default:
		return fmt.Errorf("build source entry %q has unsupported type %s", name, info.Mode().Type())
	}
}

func (snapshot *sourceSnapshot) charge(bytes int64) error {
	if bytes > snapshot.metadataLimit-snapshot.metadataBytes {
		return errors.New("build source exceeds retained observation/name budget")
	}
	snapshot.metadataBytes += bytes
	return nil
}

func (snapshot *sourceSnapshot) readNames(ctx context.Context, directory *os.File, name string) ([]string, error) {
	var names []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		children, err := directory.ReadDir(128)
		for _, child := range children {
			// Charge before retaining, including children later excluded by selection.
			if err := snapshot.charge(32 + int64(len(child.Name()))); err != nil {
				return nil, err
			}
			names = append(names, child.Name())
		}
		if err == io.EOF {
			return names, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read build source directory %q: %w", name, err)
		}
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

// Ignored observations keep the original syntax checks without requiring them
// to be representable in a destination they will never enter.
func validateObservedPath(name string) error {
	if !utf8.ValidString(name) || name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsRune(name, 0) {
		return fmt.Errorf("build source contains invalid path %q", name)
	}
	return nil
}

var errSourceChanged = errors.New("source changed during capture")
