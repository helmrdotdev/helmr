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
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	controlPlaneImportPath = internalImportPrefix + "controlplane"
	// requestBodyOwner is the control plane file that reads and decodes client
	// request bodies, so every failure is classified and described in API terms.
	requestBodyOwner = "request_json.go"
)

// requestBodyAccess is the *http.Request surface that reads client input.
var requestBodyAccess = map[string]bool{
	"Body": true, "GetBody": true, "ParseForm": true, "ParseMultipartForm": true,
	"FormValue": true, "PostFormValue": true, "FormFile": true, "MultipartReader": true,
}

// requestDecoders are the calls that decode or canonicalize JSON bytes.
var requestDecoders = map[string]bool{
	"encoding/json.NewDecoder":                   true,
	"encoding/json.Unmarshal":                    true,
	internalImportPrefix + "jsoncanon.Transform": true,
}

// controlPlaneDecodersOutsideOwner lists the control plane functions, keyed as
// Function, Type.Method or var Name, that decode JSON outside the owner. None
// reads a request body; request-derived input reaches them only after the
// owner decoded or canonicalized it, and they report fixed messages.
var controlPlaneDecodersOutsideOwner = map[string]string{
	"Server.commitCheckpointReady":            "stored checkpoint manifest",
	"Server.decodeAuthFlow":                   "sealed auth flow cookie, fixed message",
	"Server.parseCommandLogCursor":            "signed cursor, fixed message",
	"Server.parseRunTelemetryCursor":          "signed cursor, fixed message",
	"Server.publishComputerSave":              "stored object inspection",
	"Server.publishInitialComputerGeneration": "stored object inspection",
	"Server.recordCheckpointComputerObject":   "stored object inspection",
	"Server.recordComputerSaveObject":         "stored object inspection",
	"Server.workerClaimComputerCommand":       "stored command environment",
	"actorStartResultFromReceipt":             "stored idempotency receipt",
	"actorWaitIdleTimeout":                    "stored definition manifest",
	"applyRunMetadataMutation":                "stored run metadata",
	"cancelCommandInTx":                       "stored idempotency receipt",
	"checkpointCorrelationID":                 "stored checkpoint manifest",
	"computerCreateResultFromReceipt":         "stored idempotency receipt",
	"computerDeleteResultFromReceipt":         "stored idempotency receipt",
	"computerMembersParams":                   "opaque cursor, fixed message",
	"decodeAPIKeyListCursor":                  "opaque cursor, fixed message",
	"decodeActorStartObject":                  "canonical request member, fixed message",
	"decodeComputerListCursor":                "opaque cursor, fixed message",
	"decodeDefinitionListCursor":              "opaque cursor, fixed message",
	"decodeDeploymentListCursor":              "opaque cursor, fixed message",
	"decodeInvitationListCursor":              "opaque cursor, fixed message",
	"decodeProjectListCursor":                 "opaque cursor, fixed message",
	"decodeScheduleListCursor":                "opaque cursor, fixed message",
	"decodeSecretListCursor":                  "opaque cursor, fixed message",
	"decodeSessionCommand":                    "canonical request member, fixed message",
	"decodeSessionListCursor":                 "opaque cursor, fixed message",
	"decodeStartTaskRequest":                  "canonical request member, fixed message",
	"decodeTokenListCursor":                   "opaque cursor, fixed message",
	"githubOAuthProvider.Resolve":             "identity provider response",
	"githubOAuthProvider.verifiedEmails":      "identity provider response",
	"jsonObject":                              "owner-decoded value, boolean result",
	"loadInspectedComputerChild":              "stored object inspection",
	"normalizeAnnotations":                    "owner-decoded metadata, fixed message",
	"normalizeRunMetadataMutation":            "owner-canonicalized metadata patch",
	"normalizeTaskFailure":                    "owner-canonicalized details, fixed message",
	"parseCommandCompletion":                  "owner-decoded field, fixed message",
	"parseRunListCursor":                      "opaque cursor, fixed message",
	"projectComputerInstanceRestore":          "stored checkpoint manifest",
	"projectRunFailure":                       "stored run failure",
	"projectRunLogRecord":                     "stored telemetry record",
	"projectRunSnapshot":                      "stored run metadata",
	"projectRuntimeComputerSource":            "stored Computer configuration",
	"projectSessionStatus":                    "stored Session failure",
	"projectSessionTurn":                      "stored Session event",
	"recordComputerObjectLocked":              "stored object inspection",
	"rejectActorStartEmptyString":             "canonical request member, fixed message",
	"rejectActorStartNullTagElements":         "canonical request member, fixed message",
	"rejectActorStartNulls":                   "canonical request member, fixed message",
	"runFailureFromCompletion":                "stored run completion",
	"scheduleResponse":                        "stored schedule failure",
	"taskStartResultFromReceipt":              "stored idempotency receipt",
	"validateActorStartIdempotencyWire":       "canonical request member, fixed message",
}

// TestControlPlaneRequestBodiesDecodeThroughOwner keeps client-facing request
// decoding in the control plane's owner file.
func TestControlPlaneRequestBodiesDecodeThroughOwner(t *testing.T) {
	root := repositoryRoot(t)
	checker := newRequestBodyChecker(t, root)
	files := checker.controlPlaneFiles(t)
	result, err := checker.check(controlPlaneImportPath, files, controlPlaneDecodersOutsideOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range result {
		t.Error(violation)
	}
	// Fail closed when a build-constrained file is outside the checked build.
	checked := map[string]bool{}
	for _, file := range files {
		checked[filepath.Base(checker.fset.Position(file.Package).Filename)] = true
	}
	entries, err := os.ReadDir(filepath.Join(root, "internal", "controlplane"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && !checked[name] {
			t.Errorf("controlplane/%s is outside the checked linux build", name)
		}
	}
}

func TestControlPlaneRequestBodyCheckRejectsBypasses(t *testing.T) {
	checker := newRequestBodyChecker(t, repositoryRoot(t))
	for name, test := range map[string]struct {
		source string
		allow  map[string]string
		want   []string
	}{
		"aliased json import": {source: `package fixture
import js "encoding/json"
func decode(raw []byte) error { var value any; return js.Unmarshal(raw, &value) }`,
			want: []string{"decode uses encoding/json.Unmarshal"}},
		"json function value": {source: `package fixture
import "encoding/json"
var unmarshal = json.Unmarshal`,
			want: []string{"var unmarshal uses encoding/json.Unmarshal"}},
		"canonicalization": {source: `package fixture
import "github.com/helmrdotdev/helmr/internal/jsoncanon"
func canonical(raw []byte) ([]byte, error) { return jsoncanon.Transform(raw) }`,
			want: []string{"canonical uses " + internalImportPrefix + "jsoncanon.Transform"}},
		"request alias": {source: `package fixture
import "net/http"
func handle(r *http.Request) { request := r; _ = request.Body }`,
			want: []string{"handle reads net/http.Request.Body"}},
		"struct-held request": {source: `package fixture
import "net/http"
type call struct{ request *http.Request }
func (c *call) read() { _ = c.request.Body }`,
			want: []string{"call.read reads net/http.Request.Body"}},
		"embedded request": {source: `package fixture
import "net/http"
type call struct{ *http.Request }
func (c call) read() string { return c.FormValue("x") }`,
			want: []string{"call.read reads net/http.Request.FormValue"}},
		"body copy": {source: `package fixture
import "net/http"
func handle(r *http.Request) { _, _ = r.GetBody() }`,
			want: []string{"handle reads net/http.Request.GetBody"}},
		"package-level func literal": {source: `package fixture
import "net/http"
var handle = func(r *http.Request) { _ = r.ParseForm() }`,
			want: []string{"var handle reads net/http.Request.ParseForm"}},
		"request type alias": {source: `package fixture
import "net/http"
type request = http.Request
func handle(r *request) { _ = r.Body }`,
			want: []string{"handle reads net/http.Request.Body"}},
		"request pointer alias": {source: `package fixture
import "net/http"
type request = *http.Request
func handle(r request) { _ = r.Body }`,
			want: []string{"handle reads net/http.Request.Body"}},
		"embedded request alias": {source: `package fixture
import "net/http"
type request = http.Request
type call struct{ *request }
func (c call) read() string { return c.PostFormValue("x") }`,
			want: []string{"call.read reads net/http.Request.PostFormValue"}},
		"aliased json function value": {source: `package fixture
import "encoding/json"
var decode = json.Unmarshal
func read(raw []byte) error { var value any; return decode(raw, &value) }`,
			want: []string{"var decode uses encoding/json.Unmarshal"}},
		"wrong receiver key": {source: `package fixture
import "encoding/json"
type provider struct{}
func (provider) decode(raw []byte) { var value any; _ = json.Unmarshal(raw, &value) }`,
			allow: map[string]string{"Server.decode": "stored data"},
			want:  []string{"provider.decode uses encoding/json.Unmarshal", "exception Server.decode matches no JSON decoder"}},
	} {
		t.Run(name, func(t *testing.T) {
			file, err := parser.ParseFile(checker.fset, "fixture.go", test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			violations, err := checker.check("fixture", []*ast.File{file}, test.allow)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				if !slices.ContainsFunc(violations, func(violation string) bool { return strings.Contains(violation, want) }) {
					t.Errorf("violations = %v, want one containing %q", violations, want)
				}
			}
		})
	}
	file, err := parser.ParseFile(checker.fset, "fixture.go", `package fixture
import "net/http"
func read(response *http.Response) { _ = response.Body }`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if violations, err := checker.check("fixture", []*ast.File{file}, nil); err != nil || len(violations) != 0 {
		t.Fatalf("response body violations = %v, %v", violations, err)
	}
}

type requestBodyChecker struct {
	root     string
	fset     *token.FileSet
	importer types.Importer
}

func newRequestBodyChecker(t *testing.T, root string) *requestBodyChecker {
	t.Helper()
	output := goList(t, root, "linux", "-export", "-deps", "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}", "./internal/controlplane")
	exports := map[string]string{}
	for _, line := range strings.Fields(output) {
		path, export, ok := strings.Cut(line, "=")
		if ok {
			exports[path] = export
		}
	}
	checker := &requestBodyChecker{root: root, fset: token.NewFileSet()}
	checker.importer = importer.ForCompiler(checker.fset, "gc", func(path string) (io.ReadCloser, error) {
		export, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(export)
	})
	return checker
}

func (c *requestBodyChecker) controlPlaneFiles(t *testing.T) []*ast.File {
	t.Helper()
	output := goList(t, c.root, "linux", "-f", "{{.Dir}}\n{{join .GoFiles \"\\n\"}}", "./internal/controlplane")
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

// check reports request body access and JSON decoding outside the owner file,
// except decoding in functions listed in allow, and entries of allow that no
// longer match a decoder.
func (c *requestBodyChecker) check(path string, files []*ast.File, allow map[string]string) ([]string, error) {
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	config := types.Config{Importer: c.importer}
	if _, err := config.Check(path, c.fset, files, info); err != nil {
		return nil, err
	}
	owners := declarationOwners(files)
	var violations []string
	matched := map[string]bool{}
	locate := func(pos token.Pos) (string, bool) {
		position := c.fset.Position(pos)
		if filepath.Base(position.Filename) == requestBodyOwner {
			return "", false
		}
		for _, owner := range owners {
			if owner.pos <= pos && pos < owner.end {
				return owner.name, true
			}
		}
		return "file scope", true
	}
	for ident, object := range info.Uses {
		function, ok := object.(*types.Func)
		if !ok || function.Pkg() == nil || !requestDecoders[function.Pkg().Path()+"."+function.Name()] {
			continue
		}
		if _, ok := function.Type().(*types.Signature); !ok || function.Type().(*types.Signature).Recv() != nil {
			continue
		}
		name, outside := locate(ident.Pos())
		if !outside {
			continue
		}
		if _, ok := allow[name]; ok {
			matched[name] = true
			continue
		}
		violations = append(violations, fmt.Sprintf("%s: %s uses %s.%s outside %s", c.fset.Position(ident.Pos()), name, function.Pkg().Path(), function.Name(), requestBodyOwner))
	}
	for selector, selection := range info.Selections {
		if !requestBodyAccess[selection.Obj().Name()] || !isHTTPRequest(selectedFrom(selection)) {
			continue
		}
		if name, outside := locate(selector.Sel.Pos()); outside {
			violations = append(violations, fmt.Sprintf("%s: %s reads net/http.Request.%s outside %s", c.fset.Position(selector.Sel.Pos()), name, selection.Obj().Name(), requestBodyOwner))
		}
	}
	for name := range allow {
		if !matched[name] {
			violations = append(violations, fmt.Sprintf("exception %s matches no JSON decoder outside %s", name, requestBodyOwner))
		}
	}
	slices.Sort(violations)
	return violations, nil
}

// selectedFrom returns the type that declares the selected field or method,
// following embedded fields.
func selectedFrom(selection *types.Selection) types.Type {
	if function, ok := selection.Obj().(*types.Func); ok {
		return function.Type().(*types.Signature).Recv().Type()
	}
	current := selection.Recv()
	index := selection.Index()
	for _, step := range index[:len(index)-1] {
		structure, ok := dereference(current).Underlying().(*types.Struct)
		if !ok {
			return nil
		}
		current = structure.Field(step).Type()
	}
	return current
}

func isHTTPRequest(value types.Type) bool {
	if value == nil {
		return false
	}
	named, ok := dereference(value).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "net/http" && named.Obj().Name() == "Request"
}

// dereference resolves aliases and one pointer, so aliases of the request
// type or of a pointer to it name the same type.
func dereference(value types.Type) types.Type {
	value = types.Unalias(value)
	if pointer, ok := value.(*types.Pointer); ok {
		return types.Unalias(pointer.Elem())
	}
	return value
}

type declarationOwner struct {
	pos, end token.Pos
	name     string
}

// declarationOwners names each top-level function as Function or
// Type.Method, and each package-level variable as var Name.
func declarationOwners(files []*ast.File) []declarationOwner {
	var owners []declarationOwner
	for _, file := range files {
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
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					owners = append(owners, declarationOwner{pos: value.Pos(), end: value.End(), name: "var " + value.Names[0].Name})
				}
			}
		}
	}
	return owners
}

func receiverTypeName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.StarExpr:
		return receiverTypeName(expression.X)
	case *ast.IndexExpr:
		return receiverTypeName(expression.X)
	case *ast.IndexListExpr:
		return receiverTypeName(expression.X)
	case *ast.Ident:
		return expression.Name
	default:
		return fmt.Sprintf("%T", expression)
	}
}
