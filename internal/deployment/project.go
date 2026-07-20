package deployment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
)

const (
	maxManagerProjectEntries            = 100_000
	maxManagerProjectLogicalBytes int64 = 320 << 20
	managerProjectMountPath             = "/opt/helmr/project"
)

type managerProjectEntry struct {
	path    string
	mode    fs.FileMode
	content []byte
}

func dependencyProjectEntries(source DependencySource) ([]managerProjectEntry, error) {
	if err := validateDependencySource(source); err != nil {
		return nil, err
	}
	return dependencyProjectLayout(source, true)
}

func validateDependencyProject(source DependencySource) error {
	_, err := dependencyProjectLayout(source, false)
	return err
}

func dependencyProjectLayout(
	source DependencySource,
	copyContent bool,
) ([]managerProjectEntry, error) {
	entries := make(map[string]managerProjectEntry, len(source.ManifestFiles)*2+1)
	addDirectory := func(name string) error {
		for name != "." {
			if existing, ok := entries[name]; ok {
				if existing.mode != 0755 {
					return invalidDependencySource(
						"project path %q has conflicting entry kinds",
						name,
					)
				}
				return nil
			}
			entries[name] = managerProjectEntry{path: name, mode: 0755}
			name = path.Dir(name)
		}
		return nil
	}
	addFile := func(name string, content []byte) error {
		if existing, ok := entries[name]; ok {
			return invalidDependencySource(
				"project path %q conflicts with mode %#o",
				name,
				existing.mode,
			)
		}
		if err := addDirectory(path.Dir(name)); err != nil {
			return err
		}
		var stored []byte
		if copyContent {
			stored = append([]byte(nil), content...)
		}
		entries[name] = managerProjectEntry{
			path:    name,
			mode:    0644,
			content: stored,
		}
		return nil
	}
	if err := addFile(source.Lockfile.Name, source.LockfileBytes); err != nil {
		return nil, err
	}
	logicalBytes := int64(len(source.LockfileBytes))
	for _, manifest := range source.ManifestFiles {
		name := "package.json"
		if manifest.PackagePath != "." {
			name = path.Join(manifest.PackagePath, "package.json")
		}
		if err := addFile(name, manifest.Bytes); err != nil {
			return nil, err
		}
		logicalBytes += int64(len(manifest.Bytes))
		if logicalBytes > maxManagerProjectLogicalBytes {
			return nil, invalidDependencySource(
				"manager project logical bytes exceed %d",
				maxManagerProjectLogicalBytes,
			)
		}
	}
	if len(entries) >= maxManagerProjectEntries {
		return nil, invalidDependencySource(
			"manager project entries exceed %d",
			maxManagerProjectEntries-1,
		)
	}
	out := make([]managerProjectEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare([]byte(out[i].path), []byte(out[j].path)) < 0
	})
	return out, nil
}

func writeManagerProjectArchive(
	ctx context.Context,
	destination io.Writer,
	source DependencySource,
) error {
	if ctx == nil {
		return errors.New("manager project archive context is nil")
	}
	if destination == nil {
		return errors.New("manager project archive destination is nil")
	}
	entries, err := dependencyProjectEntries(source)
	if err != nil {
		return err
	}
	if len(entries) == 0 || len(entries) >= maxManagerProjectEntries {
		return fmt.Errorf(
			"manager project archive entry count is outside [1,%d]",
			maxManagerProjectEntries-1,
		)
	}

	directories := map[string]struct{}{".": {}}
	var previous string
	var logicalBytes int64
	var nameBytes int64 = 1
	archiveBytes := 2 * tarBlockBytes
	for index, projectEntry := range entries {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("write manager project archive: %w", err)
		}
		if index > 0 &&
			bytes.Compare([]byte(previous), []byte(projectEntry.path)) >= 0 {
			return fmt.Errorf(
				"manager project entries are duplicate or out of order at %q",
				projectEntry.path,
			)
		}
		if err := validateManagerProjectPath(projectEntry.path); err != nil {
			return fmt.Errorf(
				"manager project entry %d %q: %w",
				index,
				projectEntry.path,
				err,
			)
		}
		parent := path.Dir(projectEntry.path)
		if _, ok := directories[parent]; !ok {
			return fmt.Errorf(
				"manager project entry %q has no preceding directory parent %q",
				projectEntry.path,
				parent,
			)
		}
		entry, err := managerProjectTreeEntry(projectEntry)
		if err != nil {
			return fmt.Errorf(
				"manager project entry %d %q: %w",
				index,
				projectEntry.path,
				err,
			)
		}

		pathBytes := int64(len(projectEntry.path))
		if nameBytes > maxArtifactNameBytes-pathBytes {
			return fmt.Errorf(
				"manager project raw path bytes exceed %d",
				maxArtifactNameBytes,
			)
		}
		nameBytes += pathBytes
		if entry.Kind == artifactEntryRegular {
			if logicalBytes > maxManagerProjectLogicalBytes-entry.SizeBytes {
				return fmt.Errorf(
					"manager project logical bytes exceed %d",
					maxManagerProjectLogicalBytes,
				)
			}
			logicalBytes += entry.SizeBytes
		}

		paxBytes := int64(len(paxRecord("path", entry.Path)))
		increment := 2*tarBlockBytes + roundTarBytes(paxBytes)
		if entry.Kind == artifactEntryRegular {
			increment += roundTarBytes(entry.SizeBytes)
		}
		if archiveBytes > maxManagerProjectBytes-increment {
			return fmt.Errorf(
				"manager project archive exceeds %d bytes",
				maxManagerProjectBytes,
			)
		}
		archiveBytes += increment

		if err := writeTreeEntry(ctx, destination, entry); err != nil {
			return fmt.Errorf(
				"write manager project archive entry %q: %w",
				entry.Path,
				err,
			)
		}
		if entry.Kind == artifactEntryDirectory {
			directories[entry.Path] = struct{}{}
		}
		previous = entry.Path
	}
	var end [2 * tarBlockBytes]byte
	if _, err := destination.Write(end[:]); err != nil {
		return fmt.Errorf("write manager project archive end marker: %w", err)
	}
	return nil
}

func validateManagerProjectPath(value string) error {
	if err := validatePackagePath(value, managerProjectMountPath, true); err != nil {
		return err
	}
	if len(strings.Split(value, "/")) > maxArtifactDepth {
		return fmt.Errorf("path depth exceeds %d", maxArtifactDepth)
	}
	return nil
}

func managerProjectTreeEntry(entry managerProjectEntry) (treeEntry, error) {
	switch entry.mode {
	case 0644:
		return treeEntry{
			Path:      entry.path,
			Kind:      artifactEntryRegular,
			Mode:      0644,
			SizeBytes: int64(len(entry.content)),
			Content:   bytes.NewReader(entry.content),
		}, nil
	case 0755:
		if entry.content != nil {
			return treeEntry{}, errors.New("directory has content")
		}
		return treeEntry{
			Path: entry.path,
			Kind: artifactEntryDirectory,
			Mode: 0755,
		}, nil
	default:
		return treeEntry{}, fmt.Errorf("mode %#o is unsupported", entry.mode)
	}
}
