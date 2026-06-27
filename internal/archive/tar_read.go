package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

type TarEntryKind string

const (
	TarEntryKindFile    TarEntryKind = "file"
	TarEntryKindDir     TarEntryKind = "directory"
	TarEntryKindSymlink TarEntryKind = "symlink"
)

type TarEntry struct {
	Path       string
	Kind       TarEntryKind
	Size       int64
	Mode       int64
	ModTime    time.Time
	LinkTarget string
}

type TarReadEntry struct {
	Entry  TarEntry
	Reader io.Reader
}

var (
	ErrTarEntryNotFound    = errors.New("tar entry not found")
	ErrTarEntryNotFile     = errors.New("tar entry is not a regular file")
	ErrTarEntryNotDir      = errors.New("tar entry is not a directory")
	ErrTarEntryTooLarge    = errors.New("tar entry is too large")
	ErrTarEntryUnsupported = errors.New("tar entry type is unsupported")
)

func OpenTarEntry(body io.Reader, target string, options ExtractOptions) (TarReadEntry, error) {
	target, err := cleanTarLookupPath(target)
	if err != nil {
		return TarReadEntry{}, err
	}
	if target == "." {
		return TarReadEntry{}, ErrTarEntryNotFile
	}
	reader := tar.NewReader(body)
	var entries int
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return TarReadEntry{}, ErrTarEntryNotFound
		}
		if err != nil {
			return TarReadEntry{}, fmt.Errorf("read tar archive: %w", err)
		}
		if tarHeaderIsRootDir(header) {
			continue
		}
		name, err := validateTarReadHeader(header, options, &entries)
		if err != nil {
			return TarReadEntry{}, err
		}
		if name != target {
			continue
		}
		entry, err := tarEntryFromHeader(name, header)
		if err != nil {
			return TarReadEntry{}, err
		}
		if entry.Kind != TarEntryKindFile {
			return TarReadEntry{}, ErrTarEntryNotFile
		}
		if options.MaxBytes > 0 && header.Size > options.MaxBytes {
			return TarReadEntry{}, ErrTarEntryTooLarge
		}
		return TarReadEntry{Entry: entry, Reader: io.LimitReader(reader, header.Size)}, nil
	}
}

func StatTarEntry(body io.Reader, target string, options ExtractOptions) (TarEntry, error) {
	target, err := cleanTarLookupPath(target)
	if err != nil {
		return TarEntry{}, err
	}
	if target == "." {
		return TarEntry{Path: ".", Kind: TarEntryKindDir}, nil
	}
	reader := tar.NewReader(body)
	var entries int
	var sawChild bool
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if sawChild {
				return TarEntry{Path: target, Kind: TarEntryKindDir}, nil
			}
			return TarEntry{}, ErrTarEntryNotFound
		}
		if err != nil {
			return TarEntry{}, fmt.Errorf("read tar archive: %w", err)
		}
		if tarHeaderIsRootDir(header) {
			continue
		}
		name, err := validateTarReadHeader(header, options, &entries)
		if err != nil {
			return TarEntry{}, err
		}
		if name == target {
			return tarEntryFromHeader(name, header)
		}
		if strings.HasPrefix(name, target+"/") {
			sawChild = true
		}
	}
}

func ListTarEntries(body io.Reader, target string, options ExtractOptions) ([]TarEntry, error) {
	target, err := cleanTarLookupPath(target)
	if err != nil {
		return nil, err
	}
	reader := tar.NewReader(body)
	var entries int
	listed := map[string]TarEntry{}
	var targetExists bool
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if !targetExists && target != "." {
				return nil, ErrTarEntryNotFound
			}
			out := make([]TarEntry, 0, len(listed))
			for _, entry := range listed {
				out = append(out, entry)
			}
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tar archive: %w", err)
		}
		if tarHeaderIsRootDir(header) {
			if target == "." {
				targetExists = true
			}
			continue
		}
		name, err := validateTarReadHeader(header, options, &entries)
		if err != nil {
			return nil, err
		}
		if name == target {
			entry, err := tarEntryFromHeader(name, header)
			if err != nil {
				return nil, err
			}
			if entry.Kind != TarEntryKindDir {
				return nil, ErrTarEntryNotDir
			}
			targetExists = true
			continue
		}
		child, ok := directTarChild(target, name)
		if !ok {
			continue
		}
		targetExists = true
		entry, err := tarEntryFromHeader(name, header)
		if err != nil {
			return nil, err
		}
		if child != name {
			if _, exists := listed[child]; exists {
				continue
			}
			entry = TarEntry{Path: child, Kind: TarEntryKindDir}
		}
		listed[child] = entry
	}
}

func cleanTarLookupPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "." {
		return ".", nil
	}
	return safepath.CleanSlash(trimmed, safepath.CleanOptions{})
}

func validateTarReadHeader(header *tar.Header, options ExtractOptions, entries *int) (string, error) {
	*entries = *entries + 1
	if options.MaxEntries > 0 && *entries > options.MaxEntries {
		return "", fmt.Errorf("tar archive contains too many entries")
	}
	if hasSparseMetadata(header) {
		return "", fmt.Errorf("unsupported sparse tar archive entry %q", header.Name)
	}
	if header.Typeflag == tar.TypeReg && header.Size < 0 {
		return "", fmt.Errorf("tar archive entry %q has invalid size", header.Name)
	}
	name, err := safepath.CleanSlash(header.Name, safepath.CleanOptions{})
	if err != nil {
		return "", fmt.Errorf("unsafe tar path %q", header.Name)
	}
	return name, nil
}

func tarEntryFromHeader(name string, header *tar.Header) (TarEntry, error) {
	info := header.FileInfo()
	entry := TarEntry{
		Path:    name,
		Size:    header.Size,
		Mode:    int64(info.Mode()),
		ModTime: header.ModTime,
	}
	switch header.Typeflag {
	case tar.TypeReg:
		entry.Kind = TarEntryKindFile
	case tar.TypeDir:
		entry.Kind = TarEntryKindDir
		entry.Size = 0
	case tar.TypeSymlink:
		entry.Kind = TarEntryKindSymlink
		entry.LinkTarget = header.Linkname
		entry.Size = 0
	case tar.TypeLink:
		return TarEntry{}, fmt.Errorf("%w: hardlink %q", ErrTarEntryUnsupported, header.Name)
	default:
		return TarEntry{}, fmt.Errorf("%w: %q type %d", ErrTarEntryUnsupported, header.Name, header.Typeflag)
	}
	return entry, nil
}

func directTarChild(parent, name string) (string, bool) {
	if parent == "." {
		first, _, _ := strings.Cut(name, "/")
		return first, first != ""
	}
	if !strings.HasPrefix(name, parent+"/") {
		return "", false
	}
	rest := strings.TrimPrefix(name, parent+"/")
	first, _, _ := strings.Cut(rest, "/")
	if first == "" {
		return "", false
	}
	return path.Join(parent, first), true
}
