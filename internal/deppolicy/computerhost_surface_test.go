package deppolicy

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const computerHostImportPath = internalImportPrefix + "computerhost"

// runSideComputerHostSurface is everything of computerhost that executor, the
// Run side borrowing a mounted Computer, may use: the Run-facing types and the
// members it uses today. A new entry widens the physical owner's Run surface.
var runSideComputerHostSurface = map[string]bool{
	"MountChannel":                    true,
	"MountChannel.Channel":            true,
	"MountChannel.ReleaseSource":      true,
	"MountChannel.GrantProgramResume": true,
	"MountChannel.ChannelCredential":  true,
	"MountChannel.Mount":              true,
	"ErrMountNotFound":                true,
	"ErrControlTransport":             true,
	"SourceReleaseError":              true,
	"SourceReleaseError.Err":          true,
	"CaptureRuns":                     true,
	"CaptureRuns.Register":            true,
	"CaptureWait.Pauses":              true,
	"CaptureWait.Detach":              true,
	"MemberPause":                     true,
	"MemberPause.Context":             true,
	"MemberPause.Target":              true,
	"MemberPause.Member":              true,
	"MemberPause.Abort":               true,
	"MemberPause.Settle":              true,
	"MemberPause.Resumed":             true,
}

// TestRunSideUsesOnlyComputerHostRunSurface keeps executor on the physical
// owner's Run-facing surface. Every computerhost object executor's non-test
// source resolves to, under any import name and through selectors, composite
// literal keys or type assertions, must be on that surface.
//
// The check follows resolved objects, not method sets: converting an allowed
// value to a locally declared interface and calling through it escapes the
// member allowlist. The Run side's capability is enforced by types (a
// borrowed channel; checkout and machine close are unexported), so this test
// is defense in depth rather than the boundary itself.
func TestRunSideUsesOnlyComputerHostRunSurface(t *testing.T) {
	root := repositoryRoot(t)
	checked := map[string]bool{}
	for _, goos := range computerHostSurfaceBuilds {
		checker := newComputerHostSurfaceChecker(t, root, goos)
		files := checker.executorFiles(t)
		for _, file := range files {
			checked[filepath.Base(checker.fset.Position(file.Package).Filename)] = true
		}
		violations, err := checker.violations(files)
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		for _, violation := range violations {
			t.Errorf("%s: executor uses %s outside the Run-side surface", goos, violation)
		}
	}
	// Fail closed when a build-constrained or cgo file is outside the checked
	// builds.
	entries, err := os.ReadDir(filepath.Join(root, "internal", "executor"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !checked[name] {
			t.Errorf("executor/%s is in none of the checked builds %v; extend computerHostSurfaceBuilds to cover it", name, computerHostSurfaceBuilds)
		}
	}
}

// computerHostSurfaceBuilds are the GOOS values whose GOARCH=amd64,
// CGO_ENABLED=0 executor builds the surface check type-checks.
var computerHostSurfaceBuilds = []string{"linux", "darwin"}

func TestRunSideComputerHostSurfaceCheckRejectsBypasses(t *testing.T) {
	checker := newComputerHostSurfaceChecker(t, repositoryRoot(t), "linux")
	for name, test := range map[string]struct {
		source string
		want   string
	}{
		"second import name": {`package executor
import (
	computerhost "github.com/helmrdotdev/helmr/internal/computerhost"
	physical "github.com/helmrdotdev/helmr/internal/computerhost"
)
var _ computerhost.MountChannel
var _ physical.Server`, "Server"},
		"type assertion": {`package executor
import "github.com/helmrdotdev/helmr/internal/computerhost"
func served(value any) bool { _, ok := value.(*computerhost.PreparedMachines); return ok }`, "PreparedMachines"},
		"member of an allowed type": {`package executor
import "github.com/helmrdotdev/helmr/internal/computerhost"
func unwrap(err *computerhost.SourceReleaseError) error { return err.Unwrap() }`, "SourceReleaseError.Unwrap"},
		"dot import": {`package executor
import . "github.com/helmrdotdev/helmr/internal/computerhost"
var _ MountChannel`, "dot import"},
		"blank import": {`package executor
import _ "github.com/helmrdotdev/helmr/internal/computerhost"`, "blank import"},
	} {
		t.Run(name, func(t *testing.T) {
			file, err := parser.ParseFile(checker.fset, name+".go", test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			violations, err := checker.violations([]*ast.File{file})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(violations, func(violation string) bool { return strings.Contains(violation, test.want) }) {
				t.Fatalf("violations = %v, want one naming %q", violations, test.want)
			}
		})
	}
}

type computerHostSurfaceChecker struct {
	root     string
	goos     string
	fset     *token.FileSet
	importer types.Importer
	names    map[types.Object]string
}

// newComputerHostSurfaceChecker type-checks against the export data the Go
// toolchain builds for executor's dependencies on goos.
func newComputerHostSurfaceChecker(t *testing.T, root, goos string) *computerHostSurfaceChecker {
	t.Helper()
	output := goList(t, root, goos, "-export", "-deps", "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}", "./internal/executor")
	exports := map[string]string{}
	for _, line := range strings.Fields(output) {
		path, export, ok := strings.Cut(line, "=")
		if ok {
			exports[path] = export
		}
	}
	checker := &computerHostSurfaceChecker{root: root, goos: goos, fset: token.NewFileSet()}
	checker.importer = importer.ForCompiler(checker.fset, "gc", func(path string) (io.ReadCloser, error) {
		export, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(export)
	})
	host, err := checker.importer.Import(computerHostImportPath)
	if err != nil {
		t.Fatal(err)
	}
	checker.names = computerHostObjectNames(host)
	return checker
}

func (c *computerHostSurfaceChecker) executorFiles(t *testing.T) []*ast.File {
	t.Helper()
	output := goList(t, c.root, c.goos, "-f", "{{.Dir}}\n{{join .GoFiles \"\\n\"}}", "./internal/executor")
	lines := strings.Split(strings.TrimSpace(output), "\n")
	files := make([]*ast.File, 0, len(lines)-1)
	for _, name := range lines[1:] {
		file, err := parser.ParseFile(c.fset, filepath.Join(lines[0], name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return files
}

// violations reports computerhost imports that hide their uses and every use
// of a computerhost object outside the Run-side surface.
func (c *computerHostSurfaceChecker) violations(files []*ast.File) ([]string, error) {
	var violations []string
	for _, file := range files {
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return nil, err
			}
			if path != computerHostImportPath || imp.Name == nil {
				continue
			}
			switch imp.Name.Name {
			case ".":
				violations = append(violations, "dot import of computerhost in "+c.fset.Position(imp.Pos()).Filename)
			case "_":
				violations = append(violations, "blank import of computerhost in "+c.fset.Position(imp.Pos()).Filename)
			}
		}
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	config := types.Config{Importer: c.importer, Error: func(error) {}}
	if _, err := config.Check(internalImportPrefix+"executor", c.fset, files, info); err != nil {
		return nil, err
	}
	check := func(object types.Object, pos token.Pos) {
		if object == nil || object.Pkg() == nil || object.Pkg().Path() != computerHostImportPath {
			return
		}
		// Instantiated generic members resolve to their declared origin.
		switch member := object.(type) {
		case *types.Func:
			object = member.Origin()
		case *types.Var:
			object = member.Origin()
		}
		name, ok := c.names[object]
		if !ok {
			name = object.Name() + " (unexported or unowned)"
		}
		if !runSideComputerHostSurface[name] {
			violations = append(violations, fmt.Sprintf("computerhost.%s at %s", name, c.fset.Position(pos)))
		}
	}
	for ident, object := range info.Uses {
		check(object, ident.Pos())
	}
	for selector, selection := range info.Selections {
		check(selection.Obj(), selector.Sel.Pos())
	}
	slices.Sort(violations)
	return slices.Compact(violations), nil
}

// computerHostObjectNames names each package-level object of host, and each
// method and field of its named types, as Name or Type.Member.
func computerHostObjectNames(host *types.Package) map[types.Object]string {
	names := map[types.Object]string{}
	scope := host.Scope()
	for _, name := range scope.Names() {
		object := scope.Lookup(name)
		names[object] = name
		named, ok := object.Type().(*types.Named)
		if _, isType := object.(*types.TypeName); !isType || !ok {
			continue
		}
		for method := range named.Methods() {
			names[method] = name + "." + method.Name()
		}
		if structure, ok := named.Underlying().(*types.Struct); ok {
			for field := range structure.Fields() {
				names[field] = name + "." + field.Name()
			}
		}
	}
	return names
}

func goList(t *testing.T, root, goos string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-buildvcs=false"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v for %s: %v\n%s", args, goos, err, stderr.String())
	}
	return string(output)
}
