package deppolicy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const internalImportPrefix = "github.com/helmrdotdev/helmr/internal/"
const moduleImportPrefix = "github.com/helmrdotdev/helmr/"

func TestInternalPackageForbiddenDependencies(t *testing.T) {
	actual, err := internalPackageDependencyGraph(filepath.Join(repositoryRoot(t), "internal"))
	if err != nil {
		t.Fatal(err)
	}

	for source, targets := range map[string][]string{
		"api":               {"deployment", "identity", "org", "workerapi"},
		"artifact":          {"artifact/snapshot", "artifact/verify", "builder", "bundle", "cas", "computerhost", "controlplane", "db", "executor"},
		"artifact/snapshot": {"artifact/verify", "builder", "bundle", "db"},
		"artifact/verify":   {"builder", "bundle", "controlplane", "db"},
		"auth":              {"db", "deployment", "identity", "org", "token", "workergroup"},
		"builder":           {"computerhost", "controlplane", "db", "dispatch", "executor", "scheduler", "vm", "wire", "worker"},
		"command":           {"api", "controlplane", "deployment", "dispatch", "identity", "org", "run", "scheduler", "session", "telemetry", "token", "workerapi"},
		"bundle":            {"artifact/snapshot", "artifact/verify", "builder", "controlplane", "db", "deployment"},
		"cas":               {"cas/s3"},
		"client":            {"workerapi", "workerclient"},
		"email":             {"email/resend"},
		"executor":          {"artifact/snapshot", "artifact/verify", "controlplane", "db", "disk", "firecracker", "guestd", "nbd", "reservation", "worker"},
		"frameio":           {"api", "db", "proto/program/v0", "wire"},
		"httpclient":        {"controlplane", "db", "workerapi"},
		"deployment":        {"api", "controlplane", "dispatch", "identity", "org", "run", "scheduler", "session", "workerapi"},
		"dispatch":          {"api", "controlplane", "scheduler", "session", "token", "workerapi"},
		"identity":          {"api", "controlplane", "deployment", "dispatch", "run", "session", "workerapi"},
		"org":               {"api", "controlplane", "deployment", "dispatch", "identity", "run", "session", "workerapi"},
		"wire":              {"api", "computerhost", "controlplane", "db", "disk", "executor", "guestd"},
		"definition":        {"api", "artifact", "artifact/snapshot", "artifact/verify", "builder", "bundle", "computerhost", "controlplane", "db", "deployment", "disk", "executor", "frameio", "guestd", "nbd", "scheduler", "vm", "wire"},
		"guestd":            {"artifact/snapshot", "artifact/verify", "bundle", "computerhost", "controlplane", "db", "executor", "vm"},
		"disk":              {"api", "computerhost", "controlplane", "db", "executor", "guestd", "pgvalue", "wire"},
		"computer":          {"api", "command", "controlplane", "deployment", "dispatch", "identity", "org", "run", "scheduler", "session", "token", "workerapi"},
		"computerhost":      {"controlplane", "db", "executor", "guestd", "worker"},
		"controlplane":      {"computerhost", "eventstream", "executor", "firecracker", "guestd", "pglock"},
		"region":            {"controlplane", "org", "workergroup"},
		"run":               {"api", "command", "controlplane", "deployment", "dispatch", "identity", "org", "scheduler", "session", "telemetry", "token", "workerapi"},
		"secret":            {"command", "computer", "dispatch", "run", "session", "token"},
		"secretbinding":     {"api", "db", "definition", "deployment", "disk"},
		"session":           {"api", "command", "controlplane", "deployment", "dispatch", "identity", "org", "scheduler", "telemetry", "token", "workerapi"},
		"telemetry":         {"clickhouse"},
		"token":             {"api", "command", "controlplane", "deployment", "dispatch", "identity", "org", "scheduler", "session", "telemetry", "workerapi"},
		"workerapi":         {"controlplane", "db", "deployment", "firecracker", "identity", "org"},
		"workerclient":      {"client"},
		// Worker supply sits beneath the Computer owner, which run composes
		// and may import. TestWorkerSupplyDoesNotReachWireContracts also
		// forbids api and workerapi across the transitive closure.
		"workergroup": {"api", "command", "computer", "controlplane", "deployment", "dispatch", "identity", "org", "run", "scheduler", "session", "token", "workerapi"},
	} {
		if _, ok := actual[source]; !ok {
			t.Fatalf("dependency rule source package does not exist: %s", source)
		}
		for _, target := range targets {
			if slices.Contains(actual[source], target) {
				t.Fatalf("internal package import is forbidden: %s must not import %s", source, target)
			}
		}
	}
}

func TestLightProgramsDoNotReachDatabase(t *testing.T) {
	root := repositoryRoot(t)
	packages := []string{
		"./cmd/guestd",
		"./cmd/helmr",
		"./cmd/internal/bundle-builder",
		"./cmd/worker",
		"./internal/artifact",
		"./internal/artifact/snapshot",
		"./internal/artifact/verify",
		"./internal/builder",
		"./internal/bundle",
		"./internal/computerhost",
		"./internal/executor",
		"./internal/reservation",
		"./internal/definition",
		"./internal/hostconfig",
	}
	for _, goos := range []string{"linux", "darwin"} {
		for _, pkg := range packages {
			cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", pkg)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go list %s for %s: %v\n%s", pkg, goos, err, output)
			}
			for _, dependency := range strings.Fields(string(output)) {
				if dependency == internalImportPrefix+"db" || dependency == internalImportPrefix+"pglock" || dependency == internalImportPrefix+"pgvalue" || strings.HasPrefix(dependency, "github.com/jackc/pgx/") {
					t.Fatalf("%s must not depend on %s for %s", pkg, dependency, goos)
				}
			}
		}
	}
}

// The worker exchanges its host secret for host credentials it never verifies;
// signing and verifying them is workergroup's, in the control plane.
func TestWorkerDoesNotLinkHostCredentialSigning(t *testing.T) {
	root := repositoryRoot(t)
	for _, goos := range []string{"linux", "darwin"} {
		cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", "./cmd/worker")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list ./cmd/worker for %s: %v\n%s", goos, err, output)
		}
		for _, dependency := range strings.Fields(string(output)) {
			if strings.HasPrefix(dependency, "github.com/golang-jwt/") || dependency == internalImportPrefix+"workergroup" {
				t.Fatalf("./cmd/worker must not depend on %s for %s", dependency, goos)
			}
		}
	}
}

// Worker supply validates pool names through the workerpoolname leaf, so it
// reaches neither the public API contract nor the worker wire contract.
func TestWorkerSupplyDoesNotReachWireContracts(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := []string{
		internalImportPrefix + "api",
		internalImportPrefix + "workerapi",
	}
	for _, goos := range []string{"linux", "darwin"} {
		cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", "./internal/workergroup")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list ./internal/workergroup for %s: %v\n%s", goos, err, output)
		}
		for _, dependency := range strings.Fields(string(output)) {
			if slices.Contains(forbidden, dependency) {
				t.Fatalf("./internal/workergroup must not depend on %s for %s", dependency, goos)
			}
		}
	}
}

func TestContractPackagesDoNotReachArtifactResources(t *testing.T) {
	root := repositoryRoot(t)
	packages := []string{
		"./internal/artifact",
		"./internal/bundle",
		"./internal/definition",
		"./internal/secretbinding",
	}
	forbidden := []string{
		internalImportPrefix + "artifact/snapshot",
		internalImportPrefix + "artifact/verify",
		internalImportPrefix + "builder",
		internalImportPrefix + "db",
	}
	for _, goos := range []string{"linux", "darwin"} {
		for _, pkg := range packages {
			cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", pkg)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go list %s for %s: %v\n%s", pkg, goos, err, output)
			}
			for _, dependency := range strings.Fields(string(output)) {
				if slices.Contains(forbidden, dependency) {
					t.Fatalf("%s must not depend on %s for %s", pkg, dependency, goos)
				}
			}
		}
	}
}

func TestCLIStateIsCLIOnly(t *testing.T) {
	root := repositoryRoot(t)
	target := moduleImportPrefix + "internal/clistate"
	for _, sourceRoot := range []string{"cmd", "internal"} {
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
	for _, sourceRoot := range []string{"cmd", "internal"} {
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

// Run, Session and Token operations lock Computer and Instance rows only
// through the computer owner's fences, which keep the statements and their
// predicates in one place. Every generated query that locks computers or
// computer_instances rows is covered.
func TestComputerRowLocksStayBehindComputerFences(t *testing.T) {
	root := filepath.Join(repositoryRoot(t), "internal")
	forbidden, err := computerRowLockQueries(filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"LockSessionCloseComputer", "LockCancellationComputers", "LockCancellationInstances",
		"LockChildComputerPair", "LockComputer", "LockComputerAdmissionAuthority", "LockComputerCommandInstance",
		"LockComputerCommandWorkerAuthority", "LockComputerForDelete", "LockComputerInstance", "LockRunLeaseClaimComputer",
		"LockRunLeaseClaimInstance", "LockTokenWaitComputer", "LockWorkerComputerInstance",
	} {
		if !forbidden[query] {
			t.Fatalf("generated query %s is not recognized as a Computer row lock", query)
		}
	}
	for _, owner := range []string{"run", "session", "token"} {
		err := filepath.WalkDir(filepath.Join(root, owner), func(filename string, d fs.DirEntry, err error) error {
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
			var found error
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || found != nil {
					return found == nil
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && forbidden[selector.Sel.Name] {
					found = fmt.Errorf("%s calls %s outside the computer owner's fences", filename, selector.Sel.Name)
				}
				return true
			})
			return found
		})
		if err != nil {
			t.Fatal(err)
		}
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
