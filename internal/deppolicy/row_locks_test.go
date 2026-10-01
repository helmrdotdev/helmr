package deppolicy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	generatedQueryName = regexp.MustCompile(`^-- name: (\w+) `)
	sqlStringLiteral   = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlLineComment     = regexp.MustCompile(`--[^\n]*`)
	sqlQuotedName      = regexp.MustCompile(`"([^"]*)"`)
	sqlSchemaPrefix    = regexp.MustCompile(`\bpublic\s*\.\s*`)
	sqlOnlyModifier    = regexp.MustCompile(`\b(from|join)\s+only\b`)
	rowLockClause      = regexp.MustCompile(`\bfor\s+(?:no\s+key\s+)?update\b(?:\s+of\s+(\w+(?:\s*,\s*\w+)*))?`)
	sqlToken           = regexp.MustCompile(`\w+|,`)
	sqlFromKeyword     = regexp.MustCompile(`\bfrom\b`)
)

// sqlKeywords are words that can follow a table in a FROM or JOIN list
// without being its alias.
var sqlKeywords = map[string]bool{
	"as": true, "cross": true, "lateral": true, "except": true, "fetch": true, "for": true, "full": true, "group": true,
	"having": true, "inner": true, "intersect": true, "join": true, "left": true, "limit": true,
	"natural": true, "offset": true, "on": true, "order": true, "outer": true, "returning": true,
	"right": true, "set": true, "union": true, "using": true, "where": true, "window": true,
}

var computerRowTables = map[string]bool{"computers": true, "computer_instances": true}

// computerRowLockQueries returns the generated queries whose statements lock
// computers or computer_instances rows FOR UPDATE or FOR NO KEY UPDATE.
func computerRowLockQueries(dbDir string) (map[string]bool, error) {
	filenames, err := filepath.Glob(filepath.Join(dbDir, "*.sql.go"))
	if err != nil {
		return nil, err
	}
	queries := map[string]bool{}
	for _, filename := range filenames {
		file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				for _, value := range spec.(*ast.ValueSpec).Values {
					literal, ok := value.(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					statement, err := strconv.Unquote(literal.Value)
					if err != nil {
						return nil, err
					}
					name := generatedQueryName.FindStringSubmatch(statement)
					if name != nil && locksComputerRows(statement) {
						queries[name[1]] = true
					}
				}
			}
		}
	}
	return queries, nil
}

// locksComputerRows reports whether a statement locks computers or
// computer_instances rows. Identifiers are compared without case, quotes,
// the public schema or ONLY. Each parenthesized query (a subquery or a CTE
// body) is its own query level. A row lock clause locks the sources of its
// own level's FROM and JOIN lists: all of them without OF, and with OF the
// sources it names, a source being named by its alias when it has one and
// by its table otherwise. A locked FROM subquery locks every source inside
// it.
func locksComputerRows(statement string) bool {
	statement = sqlStringLiteral.ReplaceAllString(statement, "''")
	statement = sqlLineComment.ReplaceAllString(statement, " ")
	statement = strings.ToLower(sqlQuotedName.ReplaceAllString(statement, "$1"))
	statement = sqlSchemaPrefix.ReplaceAllString(statement, "")
	statement = sqlOnlyModifier.ReplaceAllString(statement, "$1")
	return levelLocksComputerRows(statement, false)
}

// levelLocksComputerRows analyzes one query level; locked is set when an
// enclosing level locks this level as a FROM subquery.
func levelLocksComputerRows(level string, locked bool) bool {
	outer, nested := splitQueryLevel(level)
	sources := querySources(outer)
	lockedSources := map[int]bool{}
	for n := range sources {
		lockedSources[n] = locked
	}
	for _, lock := range rowLockClause.FindAllStringSubmatch(outer, -1) {
		for n, source := range sources {
			if lock[1] == "" {
				lockedSources[n] = true
				continue
			}
			for _, target := range strings.Split(lock[1], ",") {
				if strings.TrimSpace(target) == source.name {
					lockedSources[n] = true
				}
			}
		}
	}
	lockedSubqueries := map[int]bool{}
	for n, source := range sources {
		if !lockedSources[n] {
			continue
		}
		if computerRowTables[source.table] {
			return true
		}
		if index, ok := subqueryIndex(source.table); ok {
			lockedSubqueries[index] = true
		}
	}
	for index, query := range nested {
		if levelLocksComputerRows(query, lockedSubqueries[index]) {
			return true
		}
	}
	return false
}

// querySource is a FROM or JOIN source of a query level: a table or a
// subquery placeholder, and the name a row lock clause uses for it.
type querySource struct {
	table string
	name  string
}

// querySources returns the sources of a query level's FROM and JOIN lists.
func querySources(level string) []querySource {
	from := sqlFromKeyword.FindStringIndex(level)
	if from == nil {
		return nil
	}
	var sources []querySource
	tokens := sqlToken.FindAllString(level[from[0]:], -1)
	for n := 0; n+1 < len(tokens); n++ {
		if tokens[n] != "from" && tokens[n] != "join" && tokens[n] != "," {
			continue
		}
		table := n + 1
		if tokens[table] == "lateral" && table+1 < len(tokens) {
			table++
		}
		if tokens[table] == "," || sqlKeywords[tokens[table]] {
			continue
		}
		source := querySource{table: tokens[table], name: tokens[table]}
		alias := table + 1
		if alias < len(tokens) && tokens[alias] == "as" {
			alias++
		}
		if alias < len(tokens) && tokens[alias] != "," && !sqlKeywords[tokens[alias]] {
			source.name = tokens[alias]
		}
		sources = append(sources, source)
	}
	return sources
}

// splitQueryLevel replaces every top-level parenthesized group of a query
// level with a placeholder word naming its index and returns the groups'
// contents.
func splitQueryLevel(level string) (string, []string) {
	var outer strings.Builder
	var nested []string
	depth, start := 0, 0
	for n := 0; n < len(level); n++ {
		switch c := level[n]; {
		case c == '(':
			if depth == 0 {
				start = n + 1
				fmt.Fprintf(&outer, " %s%d ", subqueryPrefix, len(nested))
			}
			depth++
		case c == ')' && depth > 0:
			depth--
			if depth == 0 {
				nested = append(nested, level[start:n])
			}
		case depth == 0:
			outer.WriteByte(c)
		}
	}
	return outer.String(), nested
}

const subqueryPrefix = "subquery_placeholder_"

func subqueryIndex(word string) (int, bool) {
	digits, ok := strings.CutPrefix(word, subqueryPrefix)
	if !ok {
		return 0, false
	}
	index, err := strconv.Atoi(digits)
	return index, err == nil
}

func TestComputerRowLockDerivation(t *testing.T) {
	for _, test := range []struct {
		name, statement string
		want            bool
	}{
		{"plain lock", `SELECT * FROM computers WHERE id=$1 FOR UPDATE`, true},
		{"no lock", `SELECT * FROM computers WHERE id=$1`, false},
		{"schema and alias", `SELECT c.* FROM public.computers AS c WHERE c.id=$1 FOR UPDATE OF c`, true},
		{"only and no key", `SELECT c.* FROM ONLY computers AS c WHERE c.id=$1 FOR NO KEY UPDATE OF c`, true},
		{"quoted and cased", `SELECT c.* FROM "public"."Computers" "C" WHERE c.id=$1 for update of "C"`, true},
		{"instance join", `SELECT i.* FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id FOR UPDATE`, true},
		{"of several with instance", `SELECT * FROM runs r JOIN computer_instances i ON i.computer_id=r.computer_id FOR UPDATE OF r, i`, true},
		{"of several without computer", `SELECT * FROM runs r JOIN computers c ON c.id=r.computer_id JOIN sessions s ON s.id=r.session_id FOR UPDATE OF r, s`, false},
		{"line breaks", "SELECT c.id,\nc.status\nFROM computers c\nJOIN environments e ON e.id=c.environment_id\nFOR UPDATE OF c", true},
		{"of table name", `SELECT computers.* FROM computers JOIN environments ON environments.id=computers.environment_id FOR UPDATE OF computers`, true},
		{"comma source", `SELECT * FROM runs r, computers c WHERE c.id=r.computer_id FOR UPDATE OF c`, true},
		{"exists subquery", `SELECT * FROM runs r WHERE EXISTS (SELECT 1 FROM computers c WHERE c.id=r.computer_id) FOR UPDATE`, false},
		{"locking subquery", `SELECT * FROM runs WHERE computer_id IN (SELECT id FROM computers WHERE environment_id=$1 FOR UPDATE)`, true},
		{"locking cte", `WITH c AS (SELECT id FROM computers WHERE id=$1 FOR UPDATE) SELECT r.* FROM runs r JOIN c ON c.id=r.computer_id FOR UPDATE OF r`, true},
		{"reading cte", `WITH c AS (SELECT id FROM computers WHERE id=$1) SELECT r.* FROM runs r JOIN c ON c.id=r.computer_id FOR UPDATE OF r`, false},
		{"locked from subquery", `SELECT * FROM (SELECT * FROM computers) c FOR UPDATE`, true},
		{"from subquery named by of", `SELECT * FROM runs r JOIN (SELECT * FROM computer_instances) i ON i.computer_id=r.computer_id FOR UPDATE OF i`, true},
		{"from subquery not named by of", `SELECT * FROM runs r JOIN (SELECT * FROM computers) c ON c.id=r.computer_id FOR UPDATE OF r`, false},
		{"alias shadows table name", `SELECT * FROM computers c JOIN runs computers ON computers.computer_id=c.id FOR UPDATE OF computers`, false},
		{"alias named by of", `SELECT * FROM computers c JOIN runs computers ON computers.computer_id=c.id FOR UPDATE OF c`, true},
		{"comment and literal", `-- FROM computers FOR UPDATE
SELECT * FROM runs WHERE note <> '(FROM computers FOR UPDATE' FOR UPDATE`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := locksComputerRows(test.statement); got != test.want {
				t.Fatalf("locksComputerRows = %v, want %v", got, test.want)
			}
		})
	}
}
