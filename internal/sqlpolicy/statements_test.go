package sqlpolicy

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const moduleImportPrefix = "github.com/helmrdotdev/helmr/"

// sqlMethods are the pgx entry points whose SQL argument is a statement.
var sqlMethods = map[string]bool{"Exec": true, "Query": true, "QueryRow": true, "Prepare": true, "Queue": true}

// sqlReceiverPackages are the packages whose types carry SQL statements to
// PostgreSQL: the driver, its pool and the generated db transaction surfaces.
var sqlReceiverPackages = map[string]bool{
	"github.com/jackc/pgx/v5":          true,
	"github.com/jackc/pgx/v5/pgxpool":  true,
	moduleImportPrefix + "internal/db": true,
}

// statementStart recognizes a constant string that is a complete statement
// even when it reaches the driver through a helper parameter.
var statementStart = regexp.MustCompile(`^\s*(?:WITH\s|SELECT\s|INSERT\s+INTO\s|UPDATE\s+[a-z_]+\s+SET\s|DELETE\s+FROM\s|SET\s+LOCAL\s|LOCK\s+TABLE\s)`)

// statement is one SQL statement a production package sends to PostgreSQL.
type statement struct {
	pkg      string // import path without the module prefix
	owner    string // enclosing declaration
	position token.Position
	sql      string // empty when the driver argument is not a compile-time constant
	method   string
}

func (s statement) site() string { return s.pkg + ":" + s.owner }

// collectStatements type-checks the Linux build of every production package
// and returns every SQL statement argument, constant or not, plus every
// constant complete statement passed to any other call.
func collectStatements(t *testing.T) []statement {
	t.Helper()
	root := repositoryRoot(t)
	exports := map[string]string{}
	for _, line := range strings.Fields(goList(t, root, "-export", "-deps", "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}", "./internal/...", "./cmd/...")) {
		path, export, ok := strings.Cut(line, "=")
		if ok {
			exports[path] = export
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		export, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(export)
	})
	var statements []statement
	seen := map[token.Position]bool{}
	listing := strings.Split(strings.TrimSpace(goList(t, root, "-f", "{{.ImportPath}}\t{{.Dir}}\t{{join .GoFiles \",\"}}", "./internal/...", "./cmd/...")), "\n")
	for _, line := range listing {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		path, dir := parts[0], parts[1]
		if skipPackage(path) {
			continue
		}
		var files []*ast.File
		for _, name := range strings.Split(parts[2], ",") {
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		if _, err := (&types.Config{Importer: imp}).Check(path, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", path, err)
		}
		pkg := strings.TrimPrefix(path, moduleImportPrefix)
		for _, file := range files {
			owners := declarationOwners(file)
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && sqlMethods[selector.Sel.Name] {
					if selection, ok := info.Selections[selector]; ok && sqlReceiver(selection.Recv()) {
						index := sqlArgumentIndex(selector.Sel.Name)
						if len(call.Args) > index {
							arg := call.Args[index]
							s := statement{pkg: pkg, position: fset.Position(arg.Pos()), method: selector.Sel.Name, owner: owners.lookup(arg.Pos())}
							s.sql = constantString(info, arg)
							seen[s.position] = true
							statements = append(statements, s)
							return true
						}
					}
				}
				for _, arg := range call.Args {
					position := fset.Position(arg.Pos())
					if seen[position] {
						continue
					}
					if sql := constantString(info, arg); statementStart.MatchString(sql) && !strings.Contains(sql, "%") {
						seen[position] = true
						statements = append(statements, statement{pkg: pkg, position: position, owner: owners.lookup(arg.Pos()), sql: sql, method: "argument"})
					}
				}
				return true
			})
		}
	}
	sort.Slice(statements, func(i, j int) bool {
		if statements[i].position.Filename != statements[j].position.Filename {
			return statements[i].position.Filename < statements[j].position.Filename
		}
		return statements[i].position.Offset < statements[j].position.Offset
	})
	return statements
}

// skipPackage excludes generated sqlc code (checked by sqlc), test fixture
// leaves and the ClickHouse client, which does not speak to PostgreSQL.
func skipPackage(path string) bool {
	return path == moduleImportPrefix+"internal/db" ||
		strings.HasSuffix(path, "test") ||
		strings.HasPrefix(path, moduleImportPrefix+"internal/clickhouse")
}

func constantString(info *types.Info, expr ast.Expr) string {
	if value := info.Types[expr].Value; value != nil && value.Kind() == constant.String {
		return constant.StringVal(value)
	}
	return ""
}

// sqlArgumentIndex locates the statement among the method arguments: Prepare
// takes (ctx, name, sql); a batch Queue takes (sql, ...); the rest (ctx, sql, ...).
func sqlArgumentIndex(method string) int {
	switch method {
	case "Prepare":
		return 2
	case "Queue":
		return 0
	}
	return 1
}

func sqlReceiver(recv types.Type) bool {
	for {
		pointer, ok := recv.(*types.Pointer)
		if !ok {
			break
		}
		recv = pointer.Elem()
	}
	named, ok := recv.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return sqlReceiverPackages[named.Obj().Pkg().Path()]
}

type declarationOwner struct {
	pos, end token.Pos
	name     string
}

type ownerIndex []declarationOwner

func declarationOwners(file *ast.File) ownerIndex {
	var owners ownerIndex
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			name := decl.Name.Name
			if decl.Recv != nil && len(decl.Recv.List) == 1 {
				name = receiverTypeName(decl.Recv.List[0].Type) + "." + name
			}
			owners = append(owners, declarationOwner{pos: decl.Pos(), end: decl.End(), name: name})
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok && len(value.Names) > 0 {
					owners = append(owners, declarationOwner{pos: value.Pos(), end: value.End(), name: "var " + value.Names[0].Name})
				}
			}
		}
	}
	return owners
}

func (owners ownerIndex) lookup(pos token.Pos) string {
	for _, owner := range owners {
		if owner.pos <= pos && pos < owner.end {
			return owner.name
		}
	}
	return "file scope"
}

func receiverTypeName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(expr.X)
	case *ast.IndexExpr:
		return receiverTypeName(expr.X)
	case *ast.IndexListExpr:
		return receiverTypeName(expr.X)
	case *ast.Ident:
		return expr.Name
	}
	return "?"
}

func goList(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-buildvcs=false"}, args...)...)
	cmd.Dir = root
	// The race harness exports GOFLAGS=-race, which Go rejects for a
	// cgo-disabled Linux listing.
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v\n%s", args, err, stderr.String())
	}
	return string(output)
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

var (
	writeTarget   = regexp.MustCompile(`(?is)\b(?:INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+(?:ONLY\s+)?([a-z_][a-z0-9_]*)`)
	rowLockClause = regexp.MustCompile(`(?is)\bFOR\s+(?:NO\s+KEY\s+)?UPDATE\b`)
	cteName       = regexp.MustCompile(`(?is)(?:WITH(?:\s+RECURSIVE)?|,)\s+([a-z_][a-z0-9_]*)\s+AS(?:\s+(?:NOT\s+)?MATERIALIZED)?\s*\(`)
)

// sqlKeywords are words the write-target scan can mistake for a table name.
var sqlKeywords = map[string]bool{"of": true, "set": true, "skip": true, "nowait": true, "where": true, "returning": true, "only": true}

// writtenTables returns the tables a statement inserts into, updates or deletes
// from, excluding CTE names it defines itself and row-lock clauses.
func writtenTables(sql string) []string {
	sql = rowLockClause.ReplaceAllString(sql, "")
	ctes := map[string]bool{}
	for _, match := range cteName.FindAllStringSubmatch(sql, -1) {
		ctes[strings.ToLower(match[1])] = true
	}
	seen := map[string]bool{}
	var tables []string
	for _, match := range writeTarget.FindAllStringSubmatch(sql, -1) {
		name := strings.ToLower(match[1])
		if ctes[name] || seen[name] || sqlKeywords[name] {
			continue
		}
		seen[name] = true
		tables = append(tables, name)
	}
	sort.Strings(tables)
	return tables
}

// splitStatements separates a simple-protocol batch into its statements.
func splitStatements(sql string) []string {
	var out []string
	for _, piece := range strings.Split(sql, ";") {
		if strings.TrimSpace(piece) != "" {
			out = append(out, piece)
		}
	}
	return out
}

// TestReportStatements prints the collected statements when SQLPOLICY_REPORT
// is set; it is the manual survey behind the ownership table, not a regression assertion.
func TestReportStatements(t *testing.T) {
	if os.Getenv("SQLPOLICY_REPORT") == "" {
		t.Skip("SQLPOLICY_REPORT is not set")
	}
	statements := collectStatements(t)
	constants, dynamic := 0, 0
	writes := map[string]map[string]int{}
	for _, s := range statements {
		if s.sql == "" {
			dynamic++
			fmt.Printf("DYNAMIC %s %s\n", s.position, s.site())
			continue
		}
		constants++
		if len(splitStatements(s.sql)) > 1 {
			fmt.Printf("MULTI %s %s\n", s.position, s.owner)
		}
		for _, table := range writtenTables(s.sql) {
			if writes[table] == nil {
				writes[table] = map[string]int{}
			}
			writes[table][s.pkg]++
		}
	}
	fmt.Printf("constants=%d dynamic=%d\n", constants, dynamic)
	tables := make([]string, 0, len(writes))
	for table := range writes {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		fmt.Printf("WRITE %s %v\n", table, writes[table])
	}
}
