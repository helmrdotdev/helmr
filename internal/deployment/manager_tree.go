package deployment

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"path"
	"strings"
)

type managerTreeEntry struct {
	kind       artifactEntryKind
	linkTarget string
}

type managerRegistryProfile struct {
	roots         map[string]struct{}
	intermediates map[string]struct{}
	binRoots      map[string]struct{}
}

type managerTreeWriter struct {
	writer io.Writer
	bytes  int64
}

func (writer *managerTreeWriter) Write(value []byte) (int, error) {
	count, err := writer.writer.Write(value)
	writer.bytes += int64(count)
	return count, err
}

func validateManagerTreeGraph(
	request ManagerRequest,
	metadata ManagerMetadata,
	graph *PackageGraph,
) error {
	if request.Operation == ManagerResolve {
		if graph != nil {
			return errors.New("manager resolve response forbids an input package graph")
		}
		return nil
	}
	if graph == nil {
		return errors.New("manager lifecycle response requires the input package graph")
	}
	canonical, err := CanonicalPackageGraph(*graph)
	if err != nil {
		return fmt.Errorf("manager lifecycle package graph: %w", err)
	}
	digest := sha256.Sum256(canonical)
	actual := "sha256:" + hex.EncodeToString(digest[:])
	if request.PackageGraph == nil ||
		request.PackageGraph.Digest != actual ||
		request.PackageGraph.SizeBytes != int64(len(canonical)) {
		return errors.New("manager lifecycle package graph does not match its request descriptor")
	}
	if metadata.Outcome == ManagerSucceeded &&
		(metadata.PackageGraphDigest == nil || *metadata.PackageGraphDigest != actual) {
		return errors.New("manager lifecycle response does not bind the input package graph")
	}
	return nil
}

func rewriteManagerTree(
	ctx context.Context,
	destination io.Writer,
	source io.Reader,
	tree ManagerTree,
	graph *PackageGraph,
) error {
	if destination == nil {
		return errors.New("manager tree destination is nil")
	}
	if source == nil {
		return errors.New("manager tree source is nil")
	}

	rawHash := sha256.New()
	limited := &io.LimitedReader{R: source, N: tree.SizeBytes}
	archive := tar.NewReader(io.TeeReader(limited, rawHash))
	entries := make(map[string]managerTreeEntry)
	canonicalHash := sha256.New()
	output := &managerTreeWriter{writer: io.MultiWriter(destination, canonicalHash)}
	sequence := managerTreeSequence(archive, entries, tree.Kind, graph)
	if err := writeTreeArchive(ctx, output, dependencyArtifact, sequence, true); err != nil {
		return err
	}
	if limited.N > 0 {
		if _, err := io.Copy(io.Discard, io.TeeReader(limited, rawHash)); err != nil {
			return err
		}
	}
	if limited.N != 0 {
		return fmt.Errorf("manager tree is %d bytes shorter than declared", limited.N)
	}
	if err := copyTreeContent(ctx, io.Discard, source, 0); err != nil {
		return fmt.Errorf("manager tree EOF: %w", err)
	}

	rawDigest := "sha256:" + hex.EncodeToString(rawHash.Sum(nil))
	if rawDigest != tree.Digest {
		return fmt.Errorf("manager tree digest = %q, want %q", rawDigest, tree.Digest)
	}
	canonicalDigest := "sha256:" + hex.EncodeToString(canonicalHash.Sum(nil))
	if output.bytes != tree.SizeBytes || canonicalDigest != rawDigest {
		return errors.New("manager tree is not the exact canonical helmr.tar.v0 stream")
	}
	return nil
}

func managerTreeSequence(
	archive *tar.Reader,
	entries map[string]managerTreeEntry,
	kind ManagerTreeKind,
	graph *PackageGraph,
) iter.Seq2[treeEntry, error] {
	return func(yield func(treeEntry, error) bool) {
		for {
			header, err := archive.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				yield(treeEntry{}, fmt.Errorf("read manager tree header: %w", err))
				return
			}
			entry, err := managerEntryFromTar(header, archive)
			if err != nil {
				yield(treeEntry{}, err)
				return
			}
			if _, exists := entries[entry.Path]; exists {
				yield(treeEntry{}, fmt.Errorf("manager tree contains duplicate path %q", entry.Path))
				return
			}
			entries[entry.Path] = managerTreeEntry{
				kind:       entry.Kind,
				linkTarget: entry.LinkTarget,
			}
			if !yield(entry, nil) {
				return
			}
		}
		if err := validateManagerTreeEntries(entries, kind, graph); err != nil {
			yield(treeEntry{}, err)
		}
	}
}

func managerEntryFromTar(header *tar.Header, content io.Reader) (treeEntry, error) {
	if header == nil {
		return treeEntry{}, errors.New("manager tree header is nil")
	}
	if header.Mode < 0 || header.Mode > math.MaxUint32 {
		return treeEntry{}, fmt.Errorf("manager tree path %q has an invalid mode", header.Name)
	}
	entry := treeEntry{
		Path:      header.Name,
		Mode:      uint32(header.Mode),
		SizeBytes: header.Size,
	}
	switch header.Typeflag {
	case tar.TypeReg:
		entry.Kind = artifactEntryRegular
		entry.Content = content
	case tar.TypeDir:
		entry.Kind = artifactEntryDirectory
	case tar.TypeSymlink:
		entry.Kind = artifactEntrySymlink
		entry.LinkTarget = header.Linkname
	default:
		return treeEntry{}, fmt.Errorf(
			"manager tree path %q has unsupported tar type %q",
			header.Name,
			header.Typeflag,
		)
	}

	expectedPAX := 1
	pathValue, hasPath := header.PAXRecords["path"]
	if entry.Kind == artifactEntrySymlink {
		expectedPAX = 2
		linkValue, hasLink := header.PAXRecords["linkpath"]
		if !hasLink || linkValue != header.Linkname {
			return treeEntry{}, fmt.Errorf(
				"manager tree symbolic link %q lacks its exact linkpath record",
				header.Name,
			)
		}
	}
	if !hasPath || pathValue != header.Name || len(header.PAXRecords) != expectedPAX {
		return treeEntry{}, fmt.Errorf(
			"manager tree path %q does not have the exact PAX record shape",
			header.Name,
		)
	}
	return entry, nil
}

func validateManagerTreeEntries(
	entries map[string]managerTreeEntry,
	kind ManagerTreeKind,
	graph *PackageGraph,
) error {
	var registry managerRegistryProfile
	switch kind {
	case ManagerOfflineStore:
		if graph != nil {
			return errors.New("offline-store tree validation forbids a package graph")
		}
	case ManagerRegistryClosure:
		if graph == nil {
			return errors.New("registry-closure tree validation requires a package graph")
		}
		registry = newManagerRegistryProfile(*graph)
		if err := validateRegistryTreeEntries(entries, registry); err != nil {
			return err
		}
	default:
		return fmt.Errorf("manager tree kind %q is unsupported", kind)
	}
	for entryPath, entry := range entries {
		if entry.kind != artifactEntrySymlink {
			continue
		}
		resolved, err := resolveManagerTreeLink(entries, entryPath)
		if err != nil {
			return fmt.Errorf("manager tree symbolic link %q: %w", entryPath, err)
		}
		if kind == ManagerRegistryClosure {
			owner := registry.owner(entryPath)
			if owner == "" || registry.owner(resolved) != owner {
				return fmt.Errorf(
					"registry-owned link %q escapes package root %q",
					entryPath,
					owner,
				)
			}
		}
	}
	return nil
}

func validateRegistryTreeEntries(
	entries map[string]managerTreeEntry,
	registry managerRegistryProfile,
) error {
	for root := range registry.roots {
		entry, exists := entries[root]
		if !exists || entry.kind != artifactEntryDirectory {
			return fmt.Errorf("registry closure package root %q is not a directory", root)
		}
	}
	for entryPath, entry := range entries {
		if entryPath == ".helmr" || strings.HasPrefix(entryPath, ".helmr/") {
			return fmt.Errorf("registry closure contains reserved path %q", entryPath)
		}
		if registry.containsBinPath(entryPath) {
			return fmt.Errorf("registry closure contains generated .bin path %q", entryPath)
		}
		if _, intermediate := registry.intermediates[entryPath]; intermediate {
			if entry.kind != artifactEntryDirectory {
				return fmt.Errorf("registry closure intermediate path %q is not a directory", entryPath)
			}
			continue
		}
		owner := registry.owner(entryPath)
		if owner == "" {
			return fmt.Errorf("registry closure contains unowned path %q", entryPath)
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(entryPath, owner), "/")
		if hasNodeModulesComponent(relative) {
			return fmt.Errorf(
				"registry package %q contains unlisted node_modules path %q",
				owner,
				entryPath,
			)
		}
	}
	return nil
}

func newManagerRegistryProfile(graph PackageGraph) managerRegistryProfile {
	profile := managerRegistryProfile{
		roots:         make(map[string]struct{}, len(graph.RegistryPackages)),
		intermediates: make(map[string]struct{}),
		binRoots:      make(map[string]struct{}),
	}
	for _, registry := range graph.RegistryPackages {
		profile.roots[registry.InstallPath] = struct{}{}
		for current := path.Dir(registry.InstallPath); current != "."; current = path.Dir(current) {
			profile.intermediates[current] = struct{}{}
		}
		profile.binRoots[registryBinRoot(registry.InstallPath)] = struct{}{}
	}
	return profile
}

func (profile managerRegistryProfile) owner(value string) string {
	for current := value; current != "."; current = path.Dir(current) {
		if _, exists := profile.roots[current]; exists {
			return current
		}
	}
	return ""
}

func (profile managerRegistryProfile) containsBinPath(value string) bool {
	for current := value; current != "."; current = path.Dir(current) {
		if _, exists := profile.binRoots[current]; exists {
			return true
		}
	}
	return false
}

func registryBinRoot(installPath string) string {
	components := strings.Split(installPath, "/")
	container := "."
	for position, component := range components {
		if component == "node_modules" {
			container = strings.Join(components[:position+1], "/")
		}
	}
	return path.Join(container, ".bin")
}

func resolveManagerTreeLink(
	entries map[string]managerTreeEntry,
	linkPath string,
) (string, error) {
	link, exists := entries[linkPath]
	if !exists || link.kind != artifactEntrySymlink {
		return "", errors.New("link entry is missing")
	}
	resolved := splitManagerTreePath(path.Dir(linkPath))
	pending := strings.Split(link.linkTarget, "/")
	visited := make(map[string]struct{})
	hops := 0
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", errors.New("link walk escapes the tree root")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidateParts := append(append([]string(nil), resolved...), component)
		candidate := strings.Join(candidateParts, "/")
		entry, exists := entries[candidate]
		if !exists {
			return "", fmt.Errorf("resolved path %q is dangling", candidate)
		}
		if entry.kind == artifactEntrySymlink {
			hops++
			if hops > maxSymlinkHops {
				return "", fmt.Errorf("link walk exceeds %d hops", maxSymlinkHops)
			}
			state := candidate + "\x00" + strings.Join(pending, "\x00")
			if _, exists := visited[state]; exists {
				return "", fmt.Errorf("link walk contains a cycle at %q", candidate)
			}
			visited[state] = struct{}{}
			pending = append(strings.Split(entry.linkTarget, "/"), pending...)
			continue
		}
		resolved = candidateParts
		if len(pending) != 0 && entry.kind != artifactEntryDirectory {
			return "", fmt.Errorf("resolved path traverses non-directory %q", candidate)
		}
	}
	if len(resolved) == 0 {
		return ".", nil
	}
	result := strings.Join(resolved, "/")
	if _, exists := entries[result]; !exists {
		return "", fmt.Errorf("resolved path %q is dangling", result)
	}
	return result, nil
}

func splitManagerTreePath(value string) []string {
	if value == "." || value == "" {
		return nil
	}
	return strings.Split(value, "/")
}
