package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultMaxExtractedBytes   = int64(512 << 20)
	defaultMaxExtractedEntries = 100000
)

type TarOptions struct {
	ExcludePatterns []string
	MaxBytes        int64
	MaxEntries      int
}

type ExtractOptions struct {
	MaxBytes   int64
	MaxEntries int
}

type ExtractStats struct {
	EntryCount int
	SizeBytes  int64
}

type Tar struct {
	Path       string
	Digest     string
	SizeBytes  int64
	EntryCount int
}

func CreateTar(root, tempDir string) (Tar, func(), error) {
	return CreateTarWithOptions(root, tempDir, TarOptions{})
}

func CreateTarWithOptions(root, tempDir string, options TarOptions) (Tar, func(), error) {
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
	writer := tar.NewWriter(io.MultiWriter(file, hash))
	stats := tarStats{}
	if err := appendTree(writer, root, excludeMatchers, options, &stats); err != nil {
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
	info, err := os.Stat(path)
	if err != nil {
		cleanup()
		return Tar{}, func() {}, fmt.Errorf("stat tar archive: %w", err)
	}
	return Tar{
		Path:       path,
		Digest:     "sha256:" + hex.EncodeToString(hash.Sum(nil)),
		SizeBytes:  info.Size(),
		EntryCount: stats.entries,
	}, cleanup, nil
}

func ExtractTar(body io.Reader, destination string) error {
	return ExtractTarWithOptions(body, destination, ExtractOptions{
		MaxBytes:   defaultMaxExtractedBytes,
		MaxEntries: defaultMaxExtractedEntries,
	})
}

func ExtractTarWithOptions(body io.Reader, destination string, options ExtractOptions) error {
	_, err := ExtractTarWithStats(body, destination, options)
	return err
}

func ExtractTarWithStats(body io.Reader, destination string, options ExtractOptions) (ExtractStats, error) {
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return ExtractStats{}, errors.New("tar archive destination is required")
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return ExtractStats{}, fmt.Errorf("create tar archive destination: %w", err)
	}
	return extractTar(body, destination, options)
}

func extractTar(body io.Reader, destination string, options ExtractOptions) (ExtractStats, error) {
	reader := tar.NewReader(body)
	var extractedBytes int64
	var extractedEntries int
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return ExtractStats{EntryCount: extractedEntries, SizeBytes: extractedBytes}, nil
		}
		if err != nil {
			return ExtractStats{}, fmt.Errorf("read tar archive: %w", err)
		}
		if tarHeaderIsRootDir(header) {
			continue
		}
		extractedEntries++
		if options.MaxEntries > 0 && extractedEntries > options.MaxEntries {
			return ExtractStats{}, fmt.Errorf("tar archive contains too many entries")
		}
		if err := validateHeaderSize(header, &extractedBytes, options.MaxBytes); err != nil {
			return ExtractStats{}, err
		}
		name, err := cleanArchiveName(header.Name)
		if err != nil {
			return ExtractStats{}, err
		}
		target, err := safeTarget(destination, name)
		if err != nil {
			return ExtractStats{}, err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := mkdirAllSafe(destination, target, fileMode(header.FileInfo().Mode(), 0o755)); err != nil {
				return ExtractStats{}, err
			}
		case tar.TypeReg:
			if err := mkdirAllSafe(destination, filepath.Dir(target), 0o755); err != nil {
				return ExtractStats{}, err
			}
			if err := writeRegularFile(target, reader, header.FileInfo().Mode(), header.Size); err != nil {
				return ExtractStats{}, err
			}
		case tar.TypeSymlink:
			if err := validateSymlinkTarget(destination, target, header.Linkname); err != nil {
				return ExtractStats{}, fmt.Errorf("unsafe symlink target for %q: %w", header.Name, err)
			}
			if err := mkdirAllSafe(destination, filepath.Dir(target), 0o755); err != nil {
				return ExtractStats{}, err
			}
			if err := os.RemoveAll(target); err != nil {
				return ExtractStats{}, err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return ExtractStats{}, err
			}
		case tar.TypeLink:
			return ExtractStats{}, fmt.Errorf("unsupported tar hardlink entry %q", header.Name)
		default:
			return ExtractStats{}, fmt.Errorf("unsupported tar archive entry %q type %d", header.Name, header.Typeflag)
		}
	}
}

func tarHeaderIsRootDir(header *tar.Header) bool {
	if header == nil || header.Typeflag != tar.TypeDir {
		return false
	}
	name := strings.TrimSpace(header.Name)
	if name == "" || path.IsAbs(name) || filepath.IsAbs(name) {
		return false
	}
	return path.Clean(filepath.ToSlash(name)) == "."
}

func validateHeaderSize(header *tar.Header, extractedBytes *int64, maxExtractedBytes int64) error {
	switch header.Typeflag {
	case tar.TypeReg:
		return ValidateTarRegularFileSize(header, extractedBytes, maxExtractedBytes)
	}
	return nil
}

func ValidateTarRegularFileSize(header *tar.Header, extractedBytes *int64, maxExtractedBytes int64) error {
	if hasSparseMetadata(header) {
		return fmt.Errorf("unsupported sparse tar archive entry %q", header.Name)
	}
	if header.Size < 0 {
		return fmt.Errorf("tar archive entry %q has invalid size", header.Name)
	}
	if maxExtractedBytes <= 0 {
		*extractedBytes += header.Size
		return nil
	}
	if header.Size > maxExtractedBytes {
		return fmt.Errorf("tar archive entry %q exceeds extracted size limit", header.Name)
	}
	if *extractedBytes > maxExtractedBytes-header.Size {
		return fmt.Errorf("tar archive exceeds extracted size limit")
	}
	*extractedBytes += header.Size
	return nil
}

func hasSparseMetadata(header *tar.Header) bool {
	for key := range header.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") || strings.HasPrefix(key, "SCHILY.realsize") {
			return true
		}
	}
	return false
}

type tarStats struct {
	entries int
	bytes   int64
}

func appendTree(writer *tar.Writer, root string, excludeMatchers []*regexp.Regexp, options TarOptions, stats *tarStats) error {
	return filepath.WalkDir(root, func(pathname string, entry os.DirEntry, walkErr error) error {
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
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stats.entries++
		if options.MaxEntries > 0 && stats.entries > options.MaxEntries {
			return fmt.Errorf("tar archive contains too many entries")
		}
		linkname := ""
		if info.Mode()&os.ModeSymlink != 0 {
			linkname, err = os.Readlink(pathname)
			if err != nil {
				return err
			}
		}
		if info.Mode().IsRegular() {
			if err := validateAppendSize(rel, info.Size(), options.MaxBytes, stats); err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, linkname)
		if err != nil {
			return err
		}
		normalizeHeader(header, rel)
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(pathname)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
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

func normalizeHeader(header *tar.Header, name string) {
	header.Name = name
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
		for _, target := range targets {
			if matcher.MatchString(target) {
				return true
			}
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

func cleanArchiveName(raw string) (string, error) {
	if strings.ContainsRune(raw, '\x00') {
		return "", fmt.Errorf("tar archive entry %q contains NUL", raw)
	}
	if raw == "" || path.IsAbs(raw) {
		return "", fmt.Errorf("unsafe tar path %q", raw)
	}
	for _, part := range strings.Split(filepath.ToSlash(raw), "/") {
		if part == ".." {
			return "", fmt.Errorf("unsafe tar path %q", raw)
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe tar path %q", raw)
	}
	return clean, nil
}

func safeTarget(destination, name string) (string, error) {
	target := filepath.Join(destination, filepath.FromSlash(name))
	rel, err := filepath.Rel(destination, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	return target, nil
}

func validateSymlinkTarget(destination string, target string, linkname string) error {
	if strings.ContainsRune(linkname, '\x00') {
		return errors.New("target contains NUL")
	}
	if linkname == "" || path.IsAbs(linkname) || filepath.IsAbs(linkname) {
		return fmt.Errorf("unsafe target path %q", linkname)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(linkname)))
	rel, err := filepath.Rel(destination, resolved)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("unsafe target path %q", linkname)
	}
	return nil
}

func mkdirAllSafe(root, dir string, mode os.FileMode) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("unsafe tar parent %q", current)
		}
	}
	return nil
}

func writeRegularFile(target string, reader io.Reader, mode os.FileMode, size int64) error {
	mode = fileMode(mode, 0o644)
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, io.LimitReader(reader, size))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return closeErr
	}
	return nil
}

func fileMode(mode os.FileMode, fallback os.FileMode) os.FileMode {
	mode &= os.ModePerm
	if mode == 0 {
		return fallback
	}
	return mode
}
