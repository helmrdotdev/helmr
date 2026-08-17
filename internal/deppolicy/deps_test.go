package deppolicy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const internalImportPrefix = "github.com/helmrdotdev/helmr/internal/"
const moduleImportPrefix = "github.com/helmrdotdev/helmr/"

func TestOperatorAPIDependencyBudget(t *testing.T) {
	imports, err := packageImports(filepath.Join(repositoryRoot(t), "capacityapi"))
	if err != nil {
		t.Fatal(err)
	}
	for _, importPath := range imports {
		first, _, _ := strings.Cut(importPath, "/")
		if strings.Contains(first, ".") {
			t.Fatalf("capacityapi must depend only on the standard library; imports %s", importPath)
		}
	}
}

func TestInternalPackageForbiddenDependencies(t *testing.T) {
	actual, err := internalPackageDependencyGraph(filepath.Join(repositoryRoot(t), "internal"))
	if err != nil {
		t.Fatal(err)
	}

	for source, targets := range map[string][]string{
		"api":          {"workerapi"},
		"auth":         {"db", "token"},
		"cas":          {"cas/s3"},
		"client":       {"workerapi", "workerclient"},
		"email":        {"email/resend"},
		"enrollment":   {"controlplane", "db"},
		"frameio":      {"api", "db", "proto/run/v0", "wire"},
		"httpclient":   {"controlplane", "db", "workerapi"},
		"wire":         {"api", "controlplane", "db", "executor", "guestd", "workspace"},
		"deployment":   {"compute", "vm", "wire"},
		"guestd":       {"controlplane", "db", "executor", "vm"},
		"imagebuild":   {"compute", "frameio", "oci", "vm", "wire"},
		"workspace":    {"api", "controlplane", "db", "executor", "guestd", "pgvalue", "wire"},
		"controlplane": {"eventstream", "executor", "firecracker", "guestd"},
		"secret":       {"run"},
		"substrate":    {"controlplane", "db", "executor", "worker"},
		"telemetry":    {"clickhouse"},
		"workerapi":    {"controlplane", "db", "firecracker"},
		"workerclient": {"client"},
	} {
		for _, target := range targets {
			if slices.Contains(actual[source], target) {
				t.Fatalf("internal package import is forbidden: %s must not import %s", source, target)
			}
		}
	}
}

func TestCLIStateIsCLIOnly(t *testing.T) {
	root := repositoryRoot(t)
	target := moduleImportPrefix + "internal/clistate"
	for _, sourceRoot := range []string{"cmd", "internal", "capacityapi"} {
		err := filepath.WalkDir(filepath.Join(root, sourceRoot), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				importPath, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				if importPath != target {
					continue
				}
				source, err := filepath.Rel(root, filepath.Dir(path))
				if err != nil {
					return err
				}
				if source != filepath.Join("cmd", "helmr") {
					return fmt.Errorf("CLI state import is forbidden outside cmd/helmr: %s", source)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDomainPackagesUseNaturalImportNames(t *testing.T) {
	root := repositoryRoot(t)
	for _, sourceRoot := range []string{"cmd", "internal", "capacityapi"} {
		err := filepath.WalkDir(filepath.Join(root, sourceRoot), func(filename string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(filename, ".go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), filename, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				if imp.Name == nil || !strings.HasSuffix(imp.Name.Name, "domain") {
					continue
				}
				importPath, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				naturalName := strings.TrimSuffix(imp.Name.Name, "domain")
				if strings.HasPrefix(importPath, moduleImportPrefix) && strings.HasSuffix(importPath, "/"+naturalName) {
					return fmt.Errorf("domain package %s must use its natural import name in %s", importPath, filename)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunTestExcludesGenericDatabaseHelpers(t *testing.T) {
	root := filepath.Join(repositoryRoot(t), "internal", "run", "runtest")
	forbidden := map[string]bool{
		"MustExec": true,
		"Digest":   true,
		"Hash":     true,
		"ShortID":  true,
	}
	err := filepath.WalkDir(root, func(filename string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && forbidden[function.Name.Name] {
				return fmt.Errorf("run/runtest must not own generic database helper %s", function.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProviderNeutralPackagesExcludeProviderSDKs(t *testing.T) {
	root := filepath.Join(repositoryRoot(t), "internal")
	for source, forbiddenPrefixes := range map[string][]string{
		"auth":         {"github.com/jackc/pgx/"},
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
