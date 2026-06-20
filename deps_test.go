package helmr

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const internalImportPrefix = "github.com/helmrdotdev/helmr/internal/"

func TestInternalPackageDependencies(t *testing.T) {
	actual, err := internalPackageDependencyGraph("internal")
	if err != nil {
		t.Fatal(err)
	}

	expected := map[string][]string{
		"adapter":            {},
		"api":                {"compute"},
		"archive":            {"safepath", "sha256sum"},
		"auth":               {"db", "pgvalue"},
		"builder":            {"proto/bundle/v0", "secret"},
		"buildkit":           {"builder", "proto/bundle/v0", "safepath", "secret"},
		"cas":                {},
		"checkpoint":         {},
		"cli/browser":        {},
		"cli/format":         {},
		"cli/session":        {},
		"cli/ui":             {"api"},
		"client":             {"api", "sha256sum", "version"},
		"compute":            {"sha256sum"},
		"config":             {"auth"},
		"console":            {},
		"control":            {"api", "archive", "auth", "cas", "compute", "console", "db", "db/schema", "deployment", "dispatch", "email", "pgvalue", "publicaccess", "schedule", "secret", "sha256sum", "tracing", "waitpoint", "workspace"},
		"db":                 {},
		"db/dbtest":          {},
		"db/schema":          {},
		"deployment":         {"api", "archive", "builder", "cas", "compute", "proto/bundle/v0", "schedule", "secret", "task", "transport", "vm"},
		"dispatch":           {"compute", "db", "pgvalue"},
		"dispatch/redis":     {"compute", "dispatch"},
		"email":              {},
		"executor":           {"api", "archive", "builder", "cas", "checkpoint", "compute", "proto/bundle/v0", "proto/run/v0", "proto/workspace/v0", "sha256sum", "task", "transport", "vm", "workspace"},
		"firecracker":        {"cas", "compute", "sha256sum", "vm"},
		"guestd":             {"archive", "proto/run/v0", "proto/workspace/v0", "safepath", "sha256sum", "transport", "workspace"},
		"pgvalue":            {},
		"proto/bundle/v0":    {},
		"proto/run/v0":       {},
		"proto/workspace/v0": {},
		"publicaccess":       {"auth"},
		"schedule":           {"api", "db", "pgvalue"},
		"secret":             {"api", "db", "pgvalue"},
		"sha256sum":          {},
		"safepath":           {},
		"task":               {"archive", "builder", "compute", "proto/bundle/v0", "transport", "vm"},
		"tracing":            {},
		"transport":          {"proto/run/v0", "sha256sum"},
		"version":            {},
		"vm":                 {"compute"},
		"sqs":                {},
		"waitpoint":          {"auth"},
		"worker":             {"api", "client", "compute"},
		"workspace":          {"archive", "safepath"},
	}
	normalizeGraph(expected)

	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s\nactual:\n%s\nexpected:\n%s", dependencyChangeMessage(actual, expected), formatGraph(actual), formatGraph(expected))
	}
}

func internalPackageDependencyGraph(root string) (map[string][]string, error) {
	graph := make(map[string][]string)
	edges := make(map[string]map[string]struct{})

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		source, err := internalPackageName(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		if _, ok := graph[source]; !ok {
			graph[source] = nil
		}

		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return fmt.Errorf("parse import path %s: %w", path, err)
			}
			if !strings.HasPrefix(importPath, internalImportPrefix) {
				continue
			}
			target := internalImportName(importPath)
			if source == target {
				continue
			}
			if edges[source] == nil {
				edges[source] = make(map[string]struct{})
			}
			edges[source][target] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for source, targets := range edges {
		for target := range targets {
			graph[source] = append(graph[source], target)
		}
	}
	normalizeGraph(graph)
	return graph, nil
}

func internalPackageName(root, dir string) (string, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", fmt.Errorf("file in internal root")
	}
	return filepath.ToSlash(rel), nil
}

func internalImportName(importPath string) string {
	return strings.TrimPrefix(importPath, internalImportPrefix)
}

func normalizeGraph(graph map[string][]string) {
	for source, targets := range graph {
		slices.Sort(targets)
		targets = slices.Compact(targets)
		if targets == nil {
			targets = []string{}
		}
		graph[source] = targets
	}
}

func dependencyChangeMessage(actual, expected map[string][]string) string {
	const suffix = "if intentional, update the expected graph in deps_test.go"

	for _, source := range sortedGraphSources(actual) {
		targets := actual[source]
		expectedTargets, ok := expected[source]
		if !ok {
			return fmt.Sprintf("internal package imports changed: %s is new; %s", source, suffix)
		}
		for _, target := range targets {
			if !slices.Contains(expectedTargets, target) {
				return fmt.Sprintf("internal package imports changed: %s now imports %s; %s", source, target, suffix)
			}
		}
	}
	for _, source := range sortedGraphSources(expected) {
		targets := expected[source]
		actualTargets, ok := actual[source]
		if !ok {
			return fmt.Sprintf("internal package imports changed: %s is no longer present; %s", source, suffix)
		}
		for _, target := range targets {
			if !slices.Contains(actualTargets, target) {
				return fmt.Sprintf("internal package imports changed: %s no longer imports %s; %s", source, target, suffix)
			}
		}
	}
	return "internal package imports changed; if intentional, update the expected graph in deps_test.go"
}

func formatGraph(graph map[string][]string) string {
	sources := sortedGraphSources(graph)

	var b strings.Builder
	for _, source := range sources {
		b.WriteString(source)
		b.WriteString(" ->")
		for _, target := range graph[source] {
			b.WriteByte(' ')
			b.WriteString(target)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func sortedGraphSources(graph map[string][]string) []string {
	sources := make([]string, 0, len(graph))
	for source := range graph {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	return sources
}
