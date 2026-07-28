package deployment

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Masterminds/semver/v3"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/safepath"
)

const (
	maxSubmittedSourceEntries      = 100_000
	maxSubmittedSourceLogicalBytes = int64(512 << 20)
	maxSubmittedSourceIgnoreBytes  = 1 << 20
)

type SourceSelection struct {
	NodeVersion    string
	Manager        PackageManager
	LockfileName   string
	LockfileDigest string
	ConfigDigest   string
}

type sourceCandidate struct {
	kind byte
	raw  []byte
}

func InspectSource(body io.Reader) (SourceSelection, error) {
	if body == nil {
		return SourceSelection{}, errors.New("submitted source is nil")
	}
	seen := make(map[string]byte)
	candidates := make(map[string]sourceCandidate, 4)
	var paths []submittedSourcePath
	var ignoreBody []byte
	var hasIgnore bool
	var entries int
	var logicalBytes int64
	var previousName string
	var artifactBytes int64
	for {
		rawHeader, terminal, err := readSourceHeader(body, &artifactBytes)
		if err != nil {
			return SourceSelection{}, err
		}
		if terminal {
			break
		}
		header, err := parseSourceHeader(rawHeader)
		if err != nil {
			return SourceSelection{}, fmt.Errorf("read submitted source: %w", err)
		}
		if err := validateSourceHeader(header); err != nil {
			return SourceSelection{}, err
		}
		if err := validateCanonicalSourceHeader(rawHeader, header); err != nil {
			return SourceSelection{}, err
		}
		entries++
		if entries > maxSubmittedSourceEntries {
			return SourceSelection{}, fmt.Errorf(
				"submitted source entries exceed %d",
				maxSubmittedSourceEntries,
			)
		}
		emittedName := header.Name
		pathName := emittedName
		if header.Typeflag == tar.TypeDir {
			pathName = strings.TrimSuffix(pathName, "/")
		}
		name, err := safepath.CleanSlash(pathName, safepath.CleanOptions{})
		if err != nil {
			return SourceSelection{}, fmt.Errorf("submitted source path %q: %w", header.Name, err)
		}
		if name != pathName {
			return SourceSelection{}, fmt.Errorf("submitted source path %q is not canonical", header.Name)
		}
		if _, exists := seen[name]; exists {
			return SourceSelection{}, fmt.Errorf("submitted source contains duplicate path %q", name)
		}
		if parent := path.Dir(name); parent != "." {
			kind, exists := seen[parent]
			if !exists || kind != tar.TypeDir {
				return SourceSelection{}, fmt.Errorf(
					"submitted source parent %q is not an explicit directory",
					parent,
				)
			}
		}
		if previousName != "" && previousName >= emittedName {
			return SourceSelection{}, errors.New(
				"submitted source paths are not in canonical order",
			)
		}
		previousName = emittedName
		seen[name] = header.Typeflag
		if sourceRootReserved(name) {
			return SourceSelection{}, fmt.Errorf("submitted source contains reserved path %q", name)
		}
		if (header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeSymlink) &&
			archive.IsSourceSecretPath(name) {
			return SourceSelection{}, fmt.Errorf(
				"submitted source contains likely secret %q",
				name,
			)
		}
		var raw []byte
		if header.Typeflag == tar.TypeReg &&
			(sourceAuthorityPath(name) || sourceManagerConfigPath(name) ||
				name == ".helmrignore") {
			limit := int64(maxSubmittedSourceIgnoreBytes)
			if sourceManagerConfigPath(name) {
				limit = maxSourceManagerConfigBytes
			} else if sourceAuthorityPath(name) {
				switch name {
				case "package.json":
					limit = int64(maxPackageManifestSizeBytes)
				case "helmr.config.ts":
					limit = int64(maxSubmittedSourceIgnoreBytes)
				default:
					limit = maxLockfileBytes
				}
			}
			if header.Size > limit {
				return SourceSelection{}, fmt.Errorf(
					"submitted source %q exceeds %d bytes",
					name,
					limit,
				)
			}
			raw = make([]byte, header.Size)
		}
		if err := readSourcePayload(body, header.Size, raw, &artifactBytes); err != nil {
			return SourceSelection{}, fmt.Errorf("read submitted source %q: %w", name, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if name == ".helmrignore" {
				return SourceSelection{}, errors.New(
					"submitted source .helmrignore must be a regular UTF-8 file no larger than 1 MiB",
				)
			}
			if sourceManagerConfigPath(name) {
				return SourceSelection{}, fmt.Errorf(
					"submitted source %s must be a regular file no larger than 1 MiB",
					name,
				)
			}
		case tar.TypeReg:
			if header.Size < 0 || logicalBytes > maxSubmittedSourceLogicalBytes-header.Size {
				return SourceSelection{}, fmt.Errorf(
					"submitted source logical bytes exceed %d",
					maxSubmittedSourceLogicalBytes,
				)
			}
			logicalBytes += header.Size
			if sourceAuthorityPath(name) {
				candidates[name] = sourceCandidate{kind: header.Typeflag, raw: raw}
			}
			if sourceManagerConfigPath(name) {
				if err := validateSourceManagerConfig(name, raw); err != nil {
					return SourceSelection{}, err
				}
			}
			if name == ".helmrignore" {
				hasIgnore = true
				ignoreBody = raw
			}
		case tar.TypeSymlink:
			if name == ".helmrignore" {
				return SourceSelection{}, errors.New(
					"submitted source .helmrignore must be a regular UTF-8 file no larger than 1 MiB",
				)
			}
			if err := validateSourceLink(name, header.Linkname); err != nil {
				return SourceSelection{}, err
			}
			if sourceManagerConfigPath(name) {
				return SourceSelection{}, fmt.Errorf(
					"submitted source %s must be a regular file no larger than 1 MiB",
					name,
				)
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
		paths = append(paths, submittedSourcePath{
			name:  name,
			isDir: header.Typeflag == tar.TypeDir,
		})
	}
	if hasIgnore {
		if !utf8.Valid(ignoreBody) {
			return SourceSelection{}, errors.New(
				"submitted source .helmrignore must be a regular UTF-8 file no larger than 1 MiB",
			)
		}
		ignore, err := archive.ParseSourceIgnore(ignoreBody)
		if err != nil {
			return SourceSelection{}, fmt.Errorf("submitted source .helmrignore: %w", err)
		}
		for _, sourcePath := range paths {
			if sourcePath.name != ".helmrignore" &&
				ignore.Match(sourcePath.name, sourcePath.isDir) {
				return SourceSelection{}, fmt.Errorf(
					"submitted source path %q is excluded by .helmrignore",
					sourcePath.name,
				)
			}
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
	nodeVersion, err := parseSourceNode(object)
	if err != nil {
		return SourceSelection{}, err
	}
	config, ok := candidates["helmr.config.ts"]
	if !ok || config.kind != tar.TypeReg || !utf8.Valid(config.raw) {
		return SourceSelection{}, errors.New(
			"submitted source helmr.config.ts must be a regular UTF-8 file no larger than 1 MiB",
		)
	}
	lockfile, err := selectSourceLockfile(manager.Name, candidates)
	if err != nil {
		return SourceSelection{}, err
	}
	if err := validateSourceDependencies(object, manager.Name, lockfile); err != nil {
		return SourceSelection{}, err
	}
	lockfiles := 0
	for _, name := range []string{
		"package-lock.json",
		"npm-shrinkwrap.json",
		"pnpm-lock.yaml",
		"bun.lock",
	} {
		if _, exists := candidates[name]; exists {
			lockfiles++
		}
	}
	if lockfiles != 1 {
		return SourceSelection{}, errors.New(
			"submitted source must contain exactly one supported root lockfile",
		)
	}
	return SourceSelection{
		NodeVersion:    nodeVersion,
		Manager:        manager,
		LockfileName:   lockfile.name,
		LockfileDigest: digestBytes(lockfile.raw),
		ConfigDigest:   digestBytes(config.raw),
	}, nil
}

type submittedSourcePath struct {
	name  string
	isDir bool
}

func readSourceHeader(body io.Reader, artifactBytes *int64) ([]byte, bool, error) {
	header := make([]byte, tarBlockSize)
	if err := readSourceBytes(body, header, artifactBytes); err != nil {
		return nil, false, fmt.Errorf("read submitted source header: %w", err)
	}
	if !allZero(header) {
		return header, false, nil
	}
	terminal := make([]byte, tarBlockSize)
	if err := readSourceBytes(body, terminal, artifactBytes); err != nil {
		return nil, false, errors.New("submitted source has an incomplete terminal record")
	}
	if !allZero(terminal) {
		return nil, false, errors.New("submitted source has a noncanonical terminal record")
	}
	var trailing [1]byte
	n, err := body.Read(trailing[:])
	if n != 0 || err == nil {
		return nil, false, errors.New("submitted source contains bytes after its terminal records")
	}
	if !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("finish submitted source: %w", err)
	}
	return nil, true, nil
}

const tarBlockSize = 512

func parseSourceHeader(raw []byte) (*tar.Header, error) {
	reader := tar.NewReader(bytes.NewReader(raw))
	header, err := reader.Next()
	if err != nil {
		return nil, err
	}
	return header, nil
}

func validateCanonicalSourceHeader(raw []byte, header *tar.Header) error {
	canonical := &tar.Header{
		Name:     header.Name,
		Linkname: header.Linkname,
		Size:     header.Size,
		Mode:     header.Mode,
		Typeflag: header.Typeflag,
		ModTime:  time.Unix(0, 0).UTC(),
		Format:   tar.FormatUSTAR,
	}
	var encoded bytes.Buffer
	writer := tar.NewWriter(&encoded)
	if err := writer.WriteHeader(canonical); err != nil {
		return fmt.Errorf("encode canonical submitted source header %q: %w", header.Name, err)
	}
	if encoded.Len() < tarBlockSize || !bytes.Equal(raw, encoded.Bytes()[:tarBlockSize]) {
		return fmt.Errorf("submitted source path %q has noncanonical USTAR encoding", header.Name)
	}
	return nil
}

func readSourcePayload(
	body io.Reader,
	size int64,
	captured []byte,
	artifactBytes *int64,
) error {
	if size < 0 {
		return errors.New("negative entry size")
	}
	padded := size
	if remainder := padded % tarBlockSize; remainder != 0 {
		padded += tarBlockSize - remainder
	}
	var offset int64
	buffer := make([]byte, 32<<10)
	for offset < padded {
		chunk := int64(len(buffer))
		if remaining := padded - offset; remaining < chunk {
			chunk = remaining
		}
		block := buffer[:chunk]
		if err := readSourceBytes(body, block, artifactBytes); err != nil {
			return err
		}
		contentEnd := size - offset
		if contentEnd < 0 {
			contentEnd = 0
		}
		if contentEnd > chunk {
			contentEnd = chunk
		}
		if contentEnd > 0 && len(captured) > 0 {
			copy(captured[offset:offset+contentEnd], block[:contentEnd])
		}
		if contentEnd < chunk && !allZero(block[contentEnd:]) {
			return errors.New("entry padding is not zero")
		}
		offset += chunk
	}
	return nil
}

func readSourceBytes(body io.Reader, destination []byte, artifactBytes *int64) error {
	if int64(len(destination)) > archive.MaxSourceArtifactBytes-*artifactBytes {
		return fmt.Errorf(
			"submitted source artifact bytes exceed %d",
			archive.MaxSourceArtifactBytes,
		)
	}
	if _, err := io.ReadFull(body, destination); err != nil {
		return err
	}
	*artifactBytes += int64(len(destination))
	return nil
}

func allZero(value []byte) bool {
	for _, current := range value {
		if current != 0 {
			return false
		}
	}
	return true
}

func validateSourceHeader(header *tar.Header) error {
	if header == nil {
		return errors.New("submitted source header is nil")
	}
	if header.Format != tar.FormatUSTAR {
		return fmt.Errorf(
			"submitted source path %q is not encoded as USTAR",
			header.Name,
		)
	}
	switch header.Typeflag {
	case tar.TypeReg:
		if header.Size < 0 || header.Size > maxSubmittedSourceLogicalBytes ||
			header.Mode != 0o644 && header.Mode != 0o755 {
			return fmt.Errorf("submitted source path %q has noncanonical mode", header.Name)
		}
	case tar.TypeDir:
		if !strings.HasSuffix(header.Name, "/") || header.Mode != 0o755 || header.Size != 0 {
			return fmt.Errorf("submitted source directory %q has noncanonical metadata", header.Name)
		}
	case tar.TypeSymlink:
		if header.Mode != 0o777 || header.Size != 0 {
			return fmt.Errorf("submitted source link %q has noncanonical metadata", header.Name)
		}
	default:
		return fmt.Errorf(
			"submitted source path %q has unsupported type %d",
			header.Name,
			header.Typeflag,
		)
	}
	if !utf8.ValidString(header.Name) || !ustarPathRepresentable(header.Name) {
		return fmt.Errorf(
			"submitted source path %q is not USTAR-representable",
			header.Name,
		)
	}
	if header.Linkname != "" &&
		(!utf8.ValidString(header.Linkname) || len(header.Linkname) > 100) {
		return fmt.Errorf(
			"submitted source link target for %q is not USTAR-representable",
			header.Name,
		)
	}
	epoch := time.Unix(0, 0).UTC()
	if header.Uid != 0 ||
		header.Gid != 0 ||
		header.Uname != "" ||
		header.Gname != "" ||
		!header.ModTime.Equal(epoch) ||
		!header.AccessTime.IsZero() ||
		!header.ChangeTime.IsZero() ||
		header.Devmajor != 0 ||
		header.Devminor != 0 ||
		len(header.PAXRecords) != 0 ||
		header.Mode < 0 ||
		header.Mode&^0o777 != 0 {
		return fmt.Errorf(
			"submitted source path %q has noncanonical metadata",
			header.Name,
		)
	}
	return nil
}

func ustarPathRepresentable(name string) bool {
	isDirectory := strings.HasSuffix(name, "/")
	trimmed := strings.TrimSuffix(name, "/")
	index := strings.LastIndexByte(trimmed, '/')
	leaf := trimmed
	parent := ""
	if index >= 0 {
		parent = trimmed[:index]
		leaf = trimmed[index+1:]
	}
	if isDirectory {
		leaf += "/"
	}
	return leaf != "" && len(leaf) <= 100 && len(parent) <= 155
}

func sourceRootReserved(name string) bool {
	if name == ".git" || strings.HasPrefix(name, ".git/") ||
		name == "helmr" || strings.HasPrefix(name, "helmr/") ||
		name == "node_modules" || strings.HasPrefix(name, "node_modules/") {
		return true
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".helmr" {
			return true
		}
	}
	return false
}

func sourceAuthorityPath(name string) bool {
	switch name {
	case "package.json", "package-lock.json", "npm-shrinkwrap.json",
		"pnpm-lock.yaml", "bun.lock", "helmr.config.ts":
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
	name, reference, found := strings.Cut(value, "@")
	if !found || name == "" || reference == "" {
		return PackageManager{}, errors.New(
			"submitted source packageManager must be npm@<version>, pnpm@<version>, or bun@<version>",
		)
	}
	version, integrity, hasIntegrity := strings.Cut(reference, "+")
	if hasIntegrity && integrity == "" {
		return PackageManager{}, errors.New(
			"submitted source packageManager integrity is empty",
		)
	}
	manager := PackageManager{
		Integrity: integrity,
		Name:      PackageManagerName(name),
		Version:   version,
	}
	if err := ValidatePackageManager(manager); err != nil {
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
		lock, hasLock := candidates["package-lock.json"]
		shrinkwrap, hasShrinkwrap := candidates["npm-shrinkwrap.json"]
		if hasLock == hasShrinkwrap {
			return selectedSourceLockfile{}, errors.New(
				"submitted npm source must contain exactly one of package-lock.json or npm-shrinkwrap.json",
			)
		}
		if hasLock {
			if lock.kind != tar.TypeReg {
				return selectedSourceLockfile{}, errors.New(
					"submitted source package-lock.json must be a regular file",
				)
			}
			return selectedSourceLockfile{name: "package-lock.json", raw: lock.raw}, nil
		}
		if shrinkwrap.kind != tar.TypeReg {
			return selectedSourceLockfile{}, errors.New(
				"submitted source npm-shrinkwrap.json must be a regular file",
			)
		}
		return selectedSourceLockfile{name: "npm-shrinkwrap.json", raw: shrinkwrap.raw}, nil
	case PackageManagerPNPM:
		return require("pnpm-lock.yaml")
	case PackageManagerBun:
		return require("bun.lock")
	default:
		return selectedSourceLockfile{}, fmt.Errorf("submitted source manager %q is unsupported", manager)
	}
}

func parseSourceNode(manifest map[string]any) (string, error) {
	if manifest["type"] != "module" {
		return "", errors.New(`submitted source package.json type must be "module"`)
	}
	devEngines, ok := manifest["devEngines"].(map[string]any)
	if !ok {
		return "", errors.New("submitted source package.json devEngines must be an object")
	}
	runtime, ok := devEngines["runtime"].(map[string]any)
	if !ok {
		return "", errors.New("submitted source package.json devEngines.runtime must be one object")
	}
	if runtime["name"] != "node" {
		return "", errors.New(`submitted source package.json devEngines.runtime.name must be "node"`)
	}
	version, ok := runtime["version"].(string)
	if !ok {
		return "", errors.New("submitted source package.json devEngines.runtime.version must be a string")
	}
	major, minor, patch, release := parseReleaseVersion(version)
	if !release ||
		major != 22 && major != 24 ||
		major == 22 && (minor < 18 || minor == 18 && patch < 0) ||
		major == 24 && minor < 3 {
		return "", fmt.Errorf(
			"submitted source Node version %q is outside >=22.18.0 <23 or >=24.3.0 <25",
			version,
		)
	}
	if onFail, exists := runtime["onFail"]; exists && onFail != "error" {
		return "", errors.New(`submitted source package.json devEngines.runtime.onFail must be "error" when present`)
	}
	for key := range runtime {
		if key != "name" && key != "version" && key != "onFail" {
			return "", fmt.Errorf(
				"submitted source package.json devEngines.runtime contains unknown member %q",
				key,
			)
		}
	}
	if engines, exists := manifest["engines"]; exists {
		engineObject, ok := engines.(map[string]any)
		if !ok {
			return "", errors.New("submitted source package.json engines must be an object")
		}
		if node, exists := engineObject["node"]; exists {
			constraintText, ok := node.(string)
			if !ok {
				return "", errors.New("submitted source package.json engines.node must be a string")
			}
			constraint, err := semver.NewConstraint(constraintText)
			if err != nil {
				return "", fmt.Errorf("submitted source package.json engines.node: %w", err)
			}
			selected, err := semver.NewVersion(version)
			if err != nil || !constraint.Check(selected) {
				return "", fmt.Errorf(
					"submitted source Node version %s does not satisfy engines.node %q",
					version,
					constraintText,
				)
			}
		}
	}
	return version, nil
}
