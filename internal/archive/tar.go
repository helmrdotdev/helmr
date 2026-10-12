package archive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const ()

type TarOptions struct {
	// ObserveEntry receives normalized metadata in emitted order and returns
	// a sink for regular-file bytes. It applies to all archives.
	// An observer or payload error aborts and removes the archive.
	ObserveEntry      func(*tar.Header) (io.Writer, error)
	CanonicalMetadata bool
	ExcludePatterns   []string
	MaxBytes          int64
	MaxEntries        int
	// Logical regular-file bytes, retained path/link bytes, and physical stream
	// bytes have separate bounds. MaxArchiveBytes includes PAX records and padding.
	MaxNameBytes    int64
	MaxArchiveBytes int64
	MaxFileBytes    int64
}

type Tar struct {
	Path       string
	Digest     string
	SizeBytes  int64
	EntryCount int
}

func CreateTarWithOptionsContext(ctx context.Context, root, tempDir string, options TarOptions) (Tar, func(), error) {
	if err := ctx.Err(); err != nil {
		return Tar{}, func() {}, err
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return Tar{}, func() {}, errors.New("archive root is required")
	}
	excludeMatchers, err := compileExcludeMatchers(options.ExcludePatterns)
	if err != nil {
		return Tar{}, func() {}, err
	}
	if strings.TrimSpace(tempDir) != "" {
		if err := os.MkdirAll(tempDir, 0o700); err != nil {
			return Tar{}, func() {}, fmt.Errorf("create tar archive temp dir: %w", err)
		}
	}
	file, err := os.CreateTemp(tempDir, "helmr-archive-*.tar")
	if err != nil {
		return Tar{}, func() {}, fmt.Errorf("create tar archive: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	hash := sha256.New()
	var output io.Writer = io.MultiWriter(file, hash)
	if options.MaxArchiveBytes > 0 {
		output = &boundedArchiveWriter{writer: output, remaining: options.MaxArchiveBytes}
	}
	writer := tar.NewWriter(output)
	stats := tarStats{}
	appendErr := appendTree(ctx, writer, root, excludeMatchers, options, &stats)
	if appendErr != nil {
		_ = writer.Close()
		_ = file.Close()
		cleanup()
		return Tar{}, func() {}, appendErr
	}
	if err := ctx.Err(); err != nil {
		_ = writer.Close()
		_ = file.Close()
		cleanup()
		return Tar{}, func() {}, err
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		cleanup()
		return Tar{}, func() {}, fmt.Errorf("finish tar archive: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return Tar{}, func() {}, fmt.Errorf("close tar archive: %w", err)
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return Tar{}, func() {}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		cleanup()
		return Tar{}, func() {}, fmt.Errorf("stat tar archive: %w", err)
	}

	if err := ctx.Err(); err != nil {
		cleanup()
		return Tar{}, func() {}, err
	}
	return Tar{
		Path:       path,
		Digest:     sha256sum.DigestHash(hash),
		SizeBytes:  info.Size(),
		EntryCount: stats.entries,
	}, cleanup, nil
}

type tarStats struct {
	entries int
	bytes   int64
	names   int64
}

func appendTree(
	ctx context.Context,
	writer *tar.Writer,
	root string,
	excludeMatchers []*regexp.Regexp,
	options TarOptions,
	stats *tarStats,
) error {
	// Installed output has its own bounded discovery and representation rules.
	// Computer archiving keeps its existing selection and filesystem behavior.
	if options.CanonicalMetadata {
		return appendInstalledTree(ctx, writer, root, excludeMatchers, options, stats)
	}
	type pendingEntry struct {
		pathname string
		name     string
		sortKey  string
	}
	var entries []pendingEntry
	err := filepath.WalkDir(root, func(pathname string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, pathname)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if excludedRelativePath(rel, excludeMatchers) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		sortKey := rel
		entries = append(entries, pendingEntry{
			pathname: pathname,
			name:     rel,
			sortKey:  sortKey,
		})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].sortKey < entries[right].sortKey
	})
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(entry.pathname)
		if err != nil {
			return err
		}
		stats.entries++
		if options.MaxEntries > 0 && stats.entries > options.MaxEntries {
			return fmt.Errorf("tar archive contains too many entries")
		}
		linkname := ""
		if info.Mode()&os.ModeSymlink != 0 {
			linkname, err = os.Readlink(entry.pathname)
			if err != nil {
				return err
			}
		}
		if info.Mode().IsRegular() {
			if err := validateAppendSize(entry.name, info.Size(), options.MaxBytes, stats); err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, linkname)
		if err != nil {
			return err
		}
		normalizeHeader(header, entry.name, false)
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
		file, err := os.Open(entry.pathname)
		if err != nil {
			return err
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

func validateAppendSize(name string, size int64, maxBytes int64, stats *tarStats) error {
	if size < 0 {
		return fmt.Errorf("tar archive entry %q has invalid size", name)
	}
	if maxBytes <= 0 {
		stats.bytes += size
		return nil
	}
	if size > maxBytes {
		return fmt.Errorf("tar archive entry %q exceeds extracted size limit", name)
	}
	if stats.bytes > maxBytes-size {
		return fmt.Errorf("tar archive exceeds extracted size limit")
	}
	stats.bytes += size
	return nil
}

func normalizeHeader(header *tar.Header, name string, canonicalMetadata bool) {
	header.Name = filepath.ToSlash(name)
	header.Mode &= 0o777
	if header.Typeflag == tar.TypeSymlink {
		header.Mode = 0o777
	}
	if canonicalMetadata {
		executable := header.Mode&0o111 != 0
		switch header.Typeflag {
		case tar.TypeReg:
			header.Mode = 0o644
			if executable {
				header.Mode = 0o755
			}
		case tar.TypeDir:
			header.Name = strings.TrimSuffix(header.Name, "/") + "/"
			header.Mode = 0o755
		}
		header.Format = tar.FormatPAX
	}
	header.ModTime = time.Unix(0, 0)
	header.AccessTime = time.Time{}
	header.ChangeTime = time.Time{}
	header.Uid = 0
	header.Gid = 0
	header.Uname = ""
	header.Gname = ""
	header.Devmajor = 0
	header.Devminor = 0
}

func excludedRelativePath(rel string, excludeMatchers []*regexp.Regexp) bool {
	targets := []string{rel}
	if !strings.HasSuffix(rel, "/") {
		targets = append(targets, rel+"/")
	}
	for _, matcher := range excludeMatchers {
		if slices.ContainsFunc(targets, matcher.MatchString) {
			return true
		}
	}
	return false
}

func compileExcludeMatchers(patterns []string) ([]*regexp.Regexp, error) {
	matchers := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = filepath.ToSlash(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		matcher, err := regexp.Compile(globPatternSource(pattern))
		if err != nil {
			return nil, fmt.Errorf("invalid tar archive exclude pattern %q: %w", pattern, err)
		}
		matchers = append(matchers, matcher)
	}
	return matchers, nil
}

func globPatternSource(pattern string) string {
	var source strings.Builder
	source.WriteString("^")
	for index := 0; index < len(pattern); {
		char := pattern[index]
		next, hasNext := byte(0), false
		if index+1 < len(pattern) {
			next, hasNext = pattern[index+1], true
		}
		afterNext, hasAfterNext := byte(0), false
		if index+2 < len(pattern) {
			afterNext, hasAfterNext = pattern[index+2], true
		}
		switch {
		case char == '*' && hasNext && next == '*' && hasAfterNext && afterNext == '/':
			source.WriteString("(?:.*/)?")
			index += 3
		case char == '*' && hasNext && next == '*':
			source.WriteString(".*")
			index += 2
		case char == '*':
			source.WriteString("[^/]*")
			index++
		case char == '?':
			source.WriteString("[^/]")
			index++
		default:
			source.WriteString(regexp.QuoteMeta(string(char)))
			index++
		}
	}
	source.WriteString("$")
	return source.String()
}

// Enforce the physical limit at the writer, including writer.Close framing.
type boundedArchiveWriter struct {
	writer    io.Writer
	remaining int64
}

func (writer *boundedArchiveWriter) Write(body []byte) (int, error) {
	if int64(len(body)) > writer.remaining {
		return 0, errors.New("tar archive exceeds encoded size limit")
	}
	n, err := writer.writer.Write(body)
	writer.remaining -= int64(n)
	return n, err
}
