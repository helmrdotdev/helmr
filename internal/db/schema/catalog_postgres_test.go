package schema

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

const catalogSnapshot = "testdata/catalog.txt"

// TestSchemaCatalogMatchesSnapshot compares the migrated catalog with the
// checked-in snapshot, then migrates down and up again and requires the same
// catalog. The snapshot is semantic: columns are listed by name, and
// constraint names that PostgreSQL generates are omitted, so reordering or
// reformatting the migration leaves it unchanged while any change to a type,
// default, constraint, index or enum shows up in review. Regenerate it with
// HELMR_UPDATE_SCHEMA_CATALOG=1.
func TestSchemaCatalogMatchesSnapshot(t *testing.T) {
	database := dbtest.Open(t)
	if err := Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	first := dumpCatalog(t, t.Context(), database.Pool)
	if os.Getenv("HELMR_UPDATE_SCHEMA_CATALOG") == "1" {
		if err := os.MkdirAll(filepath.Dir(catalogSnapshot), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(catalogSnapshot, []byte(first), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(catalogSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if first != string(want) {
		t.Fatalf("schema catalog differs from %s; review the change and regenerate with HELMR_UPDATE_SCHEMA_CATALOG=1:\n%s", catalogSnapshot, catalogDiff(string(want), first))
	}
	if err := Down(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	if err := Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	if again := dumpCatalog(t, t.Context(), database.Pool); again != first {
		t.Fatalf("up/down/up changed the catalog:\n%s", catalogDiff(first, again))
	}
}

// generatedConstraintName matches the names PostgreSQL assigns to unnamed
// constraints, which depend on declaration order rather than meaning.
func generatedConstraintName(table, name string) bool {
	return strings.HasPrefix(name, table+"_") && generatedConstraintSuffix.MatchString(name)
}

var generatedConstraintSuffix = regexp.MustCompile(`_(pkey|key|fkey|check|excl)\d*$`)

func dumpCatalog(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var out []string
	query := func(sql string, scan func(values []any) string) {
		rows, err := pool.Query(ctx, sql)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, scan(values))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	text := func(v any) string {
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	query(`SELECT t.typname, string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
FROM pg_type t JOIN pg_enum e ON e.enumtypid = t.oid JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname = 'public' GROUP BY t.typname`, func(v []any) string {
		return fmt.Sprintf("enum %s (%s)", text(v[0]), text(v[1]))
	})
	query(`SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
       pg_get_expr(d.adbin, d.adrelid), a.attgenerated::text, a.attidentity::text
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE n.nspname = 'public' AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped
  AND c.relname <> 'schema_migrations'`, func(v []any) string {
		line := fmt.Sprintf("column %s.%s %s", text(v[0]), text(v[1]), text(v[2]))
		if v[3].(bool) {
			line += " not null"
		}
		switch text(v[5]) {
		case "s":
			line += " generated stored " + text(v[4])
		case "":
			if text(v[4]) != "" {
				line += " default " + text(v[4])
			}
		}
		if id := text(v[6]); id != "" {
			line += " identity " + id
		}
		return line
	})
	query(`SELECT c.relname, k.conname, k.contype::text, pg_get_constraintdef(k.oid, true)
FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relname <> 'schema_migrations'`, func(v []any) string {
		table, name := text(v[0]), text(v[1])
		if generatedConstraintName(table, name) {
			name = ""
		} else {
			name = " " + name
		}
		return fmt.Sprintf("constraint %s %s%s %s", table, text(v[2]), name, text(v[3]))
	})
	query(`SELECT pg_get_indexdef(i.indexrelid)
FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relname <> 'schema_migrations'
  AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = i.indexrelid)`, func(v []any) string {
		return "index " + text(v[0])
	})
	slices.Sort(out)
	return strings.Join(out, "\n") + "\n"
}

func catalogDiff(want, got string) string {
	wanted := map[string]bool{}
	for _, line := range strings.Split(want, "\n") {
		wanted[line] = true
	}
	present := map[string]bool{}
	var diff []string
	for _, line := range strings.Split(got, "\n") {
		present[line] = true
		if !wanted[line] {
			diff = append(diff, "+ "+line)
		}
	}
	for _, line := range strings.Split(want, "\n") {
		if !present[line] {
			diff = append(diff, "- "+line)
		}
	}
	slices.Sort(diff)
	if len(diff) > 200 {
		diff = append(diff[:200], fmt.Sprintf("... %d more", len(diff)-200))
	}
	return strings.Join(diff, "\n")
}
