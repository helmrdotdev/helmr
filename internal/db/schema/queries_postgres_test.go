package schema

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

// sqlc does not resolve every column inside a nested CTE. Prepare the generated
// statements on PostgreSQL as well, so a schema change cannot leave invalid SQL
// hidden behind a Go call that has not yet been exercised by a lifecycle test.
func TestQueriesPrepareWithPostgres(t *testing.T) {
	database := dbtest.Open(t)
	if err := Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	connection, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	files, err := filepath.Glob("../*.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("generated query sources are missing")
	}
	for _, path := range files {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, contents, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			constants, ok := declaration.(*ast.GenDecl)
			if !ok || constants.Tok != token.CONST {
				continue
			}
			for _, spec := range constants.Specs {
				value := spec.(*ast.ValueSpec)
				if len(value.Values) != 1 {
					continue
				}
				literal, ok := value.Values[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				sql, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(sql, "-- name:") {
					continue
				}
				t.Run(value.Names[0].Name, func(t *testing.T) {
					// LOCK is a utility statement rather than a preparable query.
					body := strings.TrimSpace(strings.SplitN(sql, "\n", 2)[1])
					if strings.HasPrefix(body, "LOCK TABLE ") {
						tx, err := connection.Begin(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						defer func() { _ = tx.Rollback(t.Context()) }()
						if _, err := tx.Exec(t.Context(), sql); err != nil {
							t.Fatal(err)
						}
						return
					}
					if _, err := connection.Conn().Prepare(t.Context(), value.Names[0].Name, sql); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
