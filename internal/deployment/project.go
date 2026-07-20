package deployment

import (
	"bytes"
	"io/fs"
	"path"
	"sort"
)

const (
	maxManagerProjectEntries            = 100_000
	maxManagerProjectLogicalBytes int64 = 320 << 20
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
