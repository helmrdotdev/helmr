package deppolicy

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const internalImportPrefix = "github.com/helmrdotdev/helmr/internal/"
const moduleImportPrefix = "github.com/helmrdotdev/helmr/"

func TestOperatorAPIDependencyBudget(t *testing.T) {
	imports, err := packageImports(filepath.Join(repositoryRoot(t), "operatorapi"))
	if err != nil {
		t.Fatal(err)
	}
	for _, importPath := range imports {
		first, _, _ := strings.Cut(importPath, "/")
		if strings.Contains(first, ".") {
			t.Fatalf("operatorapi must depend only on the standard library; imports %s", importPath)
		}
	}
}

func TestPrivatePlatformPolicyDependencies(t *testing.T) {
	imports, err := packageImports(filepath.Join(repositoryRoot(t), "cmd/internal/platform-policy"))
	if err != nil {
		t.Fatal(err)
	}
	var internalImports []string
	for _, importPath := range imports {
		if strings.HasPrefix(importPath, internalImportPrefix) {
			internalImports = append(internalImports, importPath)
		}
	}
	want := []string{moduleImportPrefix + "internal/deployment"}
	if !reflect.DeepEqual(internalImports, want) {
		t.Fatalf("platform-policy internal imports = %v, want %v", internalImports, want)
	}
}

func TestInternalPackageForbiddenDependencies(t *testing.T) {
	actual, err := internalPackageDependencyGraph(filepath.Join(repositoryRoot(t), "internal"))
	if err != nil {
		t.Fatal(err)
	}

	for source, targets := range map[string][]string{
		"api":               {"workerapi"},
		"auth":              {"db"},
		"buildkit":          {"imagebuild/worker"},
		"cas":               {"cas/s3"},
		"client":            {"workerapi", "workerclient"},
		"email":             {"email/resend"},
		"enrollment":        {"controlplane", "db"},
		"frameio":           {"api", "db", "proto/run/v0", "wire"},
		"httpclient":        {"controlplane", "db", "workerapi"},
		"wire":              {"api", "controlplane", "db", "executor", "guestd", "workspace"},
		"guestd":            {"controlplane", "db", "executor", "imagebuild/worker"},
		"imagebuild":        {"compute", "frameio", "imagebuild/worker", "imagecache", "oci", "vm", "wire"},
		"imagebuild/worker": {"controlplane", "db", "deployment", "guestd", "workerapi"},
		"imagecache/ecr":    {"imagebuild/worker"},
		"workspace":         {"api", "controlplane", "db", "executor", "guestd", "pgvalue", "wire"},
		"controlplane":      {"eventstream", "executor", "firecracker", "guestd"},
		"secret":            {"run"},
		"substrate":         {"controlplane", "db", "executor", "worker"},
		"telemetry":         {"clickhouse"},
		"workerapi":         {"controlplane", "db", "firecracker", "imagebuild/worker", "imagecache/ecr"},
		"workerclient":      {"client"},
	} {
		for _, target := range targets {
			if slices.Contains(actual[source], target) {
				t.Fatalf("internal package import is forbidden: %s must not import %s", source, target)
			}
		}
	}
}

func TestProviderNeutralPackagesExcludeProviderSDKs(t *testing.T) {
	root := filepath.Join(repositoryRoot(t), "internal")
	for source, forbiddenPrefixes := range map[string][]string{
		"cas":          {"github.com/aws/aws-sdk-go-v2/", "github.com/aws/smithy-go"},
		"controlplane": {"github.com/redis/go-redis/"},
		"email":        {"github.com/resend/resend-go/"},
		"telemetry":    {"github.com/ClickHouse/clickhouse-go/"},
	} {
		imports, err := packageImports(filepath.Join(root, source))
		if err != nil {
			t.Fatal(err)
		}
		for _, importPath := range imports {
			for _, forbiddenPrefix := range forbiddenPrefixes {
				if strings.HasPrefix(importPath, forbiddenPrefix) {
					t.Fatalf("provider-neutral package %s must not import %s", source, importPath)
				}
			}
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	workDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(workDir, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
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

func packageImports(root string) ([]string, error) {
	imports := make(map[string]struct{})
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			return filepath.SkipDir
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
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
			imports[importPath] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(imports))
	for importPath := range imports {
		result = append(result, importPath)
	}
	slices.Sort(result)
	return result, nil
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
