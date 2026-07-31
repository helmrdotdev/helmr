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
		"actor":               {"db", "ids", "outbox", "pgvalue", "run", "secret", "tracing"},
		"api":                 {"archive", "ids", "jsoncanon"},
		"archive":             {"safepath", "sha256sum"},
		"auth":                {"db", "ids", "pgvalue", "token"},
		"buildkit":            {"imagebuild", "safepath"},
		"capacity":            {},
		"cas":                 {"archive"},
		"checkpoint":          {},
		"cli/browser":         {},
		"cli/format":          {},
		"cli/session":         {},
		"cli/ui":              {"api"},
		"clickhouse":          {},
		"clickhouse/schema":   {"clickhouse"},
		"client":              {"api", "ids", "sha256sum"},
		"cmd/platform-policy": {"deployment"},
		"compute":             {"runtime/identity"},
		"config":              {"api"},
		"console":             {},
		"control":             {"actor", "api", "archive", "auth", "cas", "console", "db", "db/schema", "deployment", "email", "frameio", "idempotency", "ids", "jsoncanon", "pgvalue", "proto/run/v0", "region", "run", "runtime/identity", "schedule", "secret", "sha256sum", "telemetry", "token", "tracing", "workspace"},
		"db":                  {},
		"db/dbtest":           {},
		"db/schema":           {},
		"deployment":          {"api", "archive", "cas", "compute", "frameio", "ids", "imagebuild", "jsoncanon", "oci", "runtime/identity", "safepath", "schedule", "vm", "wire"},
		"dispatch":            {"compute", "db", "deployment", "pgvalue", "sessionlock", "workspace"},
		"dispatch/redis":      {"dispatch", "pgvalue"},
		"email":               {},
		"enrollment":          {"api", "auth", "workergroup"},
		"executor":            {"api", "capacity", "cas", "checkpoint", "client", "compute", "deployment", "frameio", "ids", "jsoncanon", "localcache", "proto/run/v0", "proto/workspace/v0", "runtime", "sha256sum", "vm", "wire", "workspace"},
		"firecracker":         {"cas", "compute", "ids", "runtime/identity", "sha256sum", "vm", "worker/datapath"},
		"fleet":               {"db", "sessionlock"},
		"frameio":             {"sha256sum"},
		"guestd":              {"archive", "deployment", "frameio", "jsoncanon", "oci", "proto/run/v0", "proto/workspace/v0", "safepath", "sha256sum", "wire", "workspace"},
		"idempotency":         {"db", "jsoncanon", "pgvalue"},
		"ids":                 {},
		"imagebuild":          {"jsoncanon"},
		"jsoncanon":           {},
		"localcache":          {},
		"oci":                 {"sha256sum"},
		"outbox":              {},
		"pgvalue":             {},
		"platformlock":        {"sessionlock"},
		"proto/run/v0":        {},
		"proto/workspace/v0":  {},
		"region":              {"db"},
		"run":                 {"db", "ids", "outbox", "pgvalue", "secret"},
		"run/runtest":         {"db/dbtest", "db/schema"},
		"runtime":             {"localcache", "oci", "sha256sum"},
		"runtime/identity":    {"sha256sum"},
		"safepath":            {},
		"schedule":            {"db", "pgvalue", "run", "tracing"},
		"secret":              {"api", "db", "idempotency", "ids", "outbox", "pgvalue"},
		"sessionlock":         {},
		"sha256sum":           {},
		"telemetry":           {"api", "clickhouse", "db", "pgvalue"},
		"token":               {"db", "ids", "outbox", "pgvalue"},
		"tracing":             {},
		"version":             {},
		"vm":                  {"compute", "ids"},
		"wire":                {"frameio", "proto/run/v0"},
		"worker":              {"api", "capacity", "client", "compute", "deployment", "ids", "vm"},
		"worker/datapath":     {},
		"workergroup":         {"auth", "db", "pgvalue"},
		"workspace":           {"archive", "jsoncanon", "proto/workspace/v0", "safepath", "sha256sum"},
	}
	normalizeGraph(expected)

	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s\nactual:\n%s\nexpected:\n%s", dependencyChangeMessage(actual, expected), formatGraph(actual), formatGraph(expected))
	}
}

func TestInternalPackageForbiddenDependencies(t *testing.T) {
	actual, err := internalPackageDependencyGraph("internal")
	if err != nil {
		t.Fatal(err)
	}

	for source, targets := range map[string][]string{
		"frameio":   {"api", "db", "proto/run/v0", "wire"},
		"wire":      {"api", "control", "db", "executor", "guestd", "workspace"},
		"guestd":    {"control", "db", "executor"},
		"workspace": {"api", "control", "db", "executor", "guestd", "pgvalue", "wire"},
		"control":   {"executor", "firecracker", "guestd"},
		"secret":    {"run"},
	} {
		for _, target := range targets {
			if slices.Contains(actual[source], target) {
				t.Fatalf("internal package import is forbidden: %s must not import %s", source, target)
			}
		}
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
