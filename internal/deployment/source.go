package deployment

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

const (
	maxSubmittedSourceEntries      = 100_000
	maxSubmittedSourceLogicalBytes = int64(512 << 20)
)

type SourceSelection struct {
	Manager        PackageManager
	LockfileName   string
	LockfileDigest string
}

type sourceCandidate struct {
	kind byte
	raw  []byte
}

func InspectSource(body io.Reader) (SourceSelection, error) {
	if body == nil {
		return SourceSelection{}, errors.New("submitted source is nil")
	}
	reader := tar.NewReader(body)
	seen := make(map[string]struct{})
	candidates := make(map[string]sourceCandidate, 4)
	var entries int
	var logicalBytes int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SourceSelection{}, fmt.Errorf("read submitted source: %w", err)
		}
		if header.Typeflag == tar.TypeDir && path.Clean(header.Name) == "." {
			continue
		}
		entries++
		if entries > maxSubmittedSourceEntries {
			return SourceSelection{}, fmt.Errorf(
				"submitted source entries exceed %d",
				maxSubmittedSourceEntries,
			)
		}
		name, err := safepath.CleanSlash(header.Name, safepath.CleanOptions{})
		if err != nil {
			return SourceSelection{}, fmt.Errorf("submitted source path %q: %w", header.Name, err)
		}
		if _, exists := seen[name]; exists {
			return SourceSelection{}, fmt.Errorf("submitted source contains duplicate path %q", name)
		}
		seen[name] = struct{}{}
		if sourceRootReserved(name) {
			return SourceSelection{}, fmt.Errorf("submitted source contains reserved path %q", name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
		case tar.TypeReg:
			if header.Size < 0 || logicalBytes > maxSubmittedSourceLogicalBytes-header.Size {
				return SourceSelection{}, fmt.Errorf(
					"submitted source logical bytes exceed %d",
					maxSubmittedSourceLogicalBytes,
				)
			}
			logicalBytes += header.Size
			if sourceAuthorityPath(name) {
				limit := int64(maxPackageManifestSizeBytes)
				if name != "package.json" {
					limit = maxLockfileBytes
				}
				if header.Size > limit {
					return SourceSelection{}, fmt.Errorf(
						"submitted source %q exceeds %d bytes",
						name,
						limit,
					)
				}
				raw, err := io.ReadAll(io.LimitReader(reader, header.Size))
				if err != nil {
					return SourceSelection{}, fmt.Errorf("read submitted source %q: %w", name, err)
				}
				if int64(len(raw)) != header.Size {
					return SourceSelection{}, fmt.Errorf("submitted source %q is truncated", name)
				}
				candidates[name] = sourceCandidate{kind: header.Typeflag, raw: raw}
			}
		case tar.TypeSymlink:
			if err := validateSourceLink(name, header.Linkname); err != nil {
				return SourceSelection{}, err
			}
			if sourceAuthorityPath(name) {
				candidates[name] = sourceCandidate{kind: header.Typeflag}
			}
		default:
			return SourceSelection{}, fmt.Errorf(
				"submitted source path %q has unsupported type %d",
				name,
				header.Typeflag,
			)
		}
	}

	manifest, ok := candidates["package.json"]
	if !ok || manifest.kind != tar.TypeReg {
		return SourceSelection{}, errors.New("submitted source package.json must be a regular file")
	}
	object, err := decodePackageManifest(manifest.raw)
	if err != nil {
		return SourceSelection{}, fmt.Errorf("submitted source package.json: %w", err)
	}
	value, ok := object["packageManager"].(string)
	if !ok {
		return SourceSelection{}, errors.New(
			"submitted source package.json packageManager must be a string",
		)
	}
	manager, err := parseSourceManager(value)
	if err != nil {
		return SourceSelection{}, err
	}
	lockfile, err := selectSourceLockfile(manager.Name, candidates)
	if err != nil {
		return SourceSelection{}, err
	}
	return SourceSelection{
		Manager:        manager,
		LockfileName:   lockfile.name,
		LockfileDigest: digestBytes(lockfile.raw),
	}, nil
}

func sourceRootReserved(name string) bool {
	return name == "helmr" || strings.HasPrefix(name, "helmr/") ||
		name == "node_modules" || strings.HasPrefix(name, "node_modules/")
}

func sourceAuthorityPath(name string) bool {
	switch name {
	case "package.json", "package-lock.json", "bun.lock", "bun.lockb":
		return true
	default:
		return false
	}
}

func validateSourceLink(name, target string) error {
	if target == "" || path.IsAbs(target) {
		return fmt.Errorf("submitted source link %q has an escaping target", name)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("submitted source link %q has an escaping target", name)
	}
	return nil
}

func parseSourceManager(value string) (PackageManager, error) {
	if value != strings.TrimSpace(value) {
		return PackageManager{}, errors.New("submitted source packageManager is not exact")
	}
	name, version, found := strings.Cut(value, "@")
	if !found || name == "" || version == "" {
		return PackageManager{}, errors.New(
			"submitted source packageManager must be npm@<version> or bun@<version>",
		)
	}
	manager := PackageManager{Name: PackageManagerName(name), Version: version}
	if err := validateManagerPackage(manager); err != nil {
		return PackageManager{}, fmt.Errorf("submitted source packageManager: %w", err)
	}
	return manager, nil
}

type selectedSourceLockfile struct {
	name string
	raw  []byte
}

func selectSourceLockfile(
	manager PackageManagerName,
	candidates map[string]sourceCandidate,
) (selectedSourceLockfile, error) {
	require := func(name string) (selectedSourceLockfile, error) {
		candidate, ok := candidates[name]
		if !ok || candidate.kind != tar.TypeReg {
			return selectedSourceLockfile{}, fmt.Errorf(
				"submitted source %s must be a regular file",
				name,
			)
		}
		return selectedSourceLockfile{name: name, raw: candidate.raw}, nil
	}
	switch manager {
	case PackageManagerNPM:
		return require("package-lock.json")
	case PackageManagerBun:
		text, hasText := candidates["bun.lock"]
		binary, hasBinary := candidates["bun.lockb"]
		if hasText == hasBinary {
			return selectedSourceLockfile{}, errors.New(
				"submitted Bun source must contain exactly one of bun.lock or bun.lockb",
			)
		}
		if hasText {
			if text.kind != tar.TypeReg {
				return selectedSourceLockfile{}, errors.New(
					"submitted source bun.lock must be a regular file",
				)
			}
			return selectedSourceLockfile{name: "bun.lock", raw: text.raw}, nil
		}
		if binary.kind != tar.TypeReg {
			return selectedSourceLockfile{}, errors.New(
				"submitted source bun.lockb must be a regular file",
			)
		}
		return selectedSourceLockfile{name: "bun.lockb", raw: binary.raw}, nil
	default:
		return selectedSourceLockfile{}, fmt.Errorf("submitted source manager %q is unsupported", manager)
	}
}
