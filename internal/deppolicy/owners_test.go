package deppolicy

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Owner edges express transaction orchestration. Reads of another owner's
// tables do not justify a reverse edge or a second writer.
// Agent and Slack admission acquire Worker supply before their channel locks.
// Command admission uses Org's Environment gate. Dispatch composes Command and
// Session failure transitions atomically when an execution admission expires.
// Identity composes account and membership revocation with Slack-owned access
// retirement in the same transaction; Slack remains the writer of its link tables.
var ownerImports = map[string][]string{
	"agent":       {"audit", "session", "slack", "slackroute", "computer", "org", "workergroup"},
	"computer":    {"audit", "secret", "workergroup"},
	"session":     {"audit", "computer", "slackroute", "secret", "org", "workergroup"},
	"dispatch":    {"audit", "session", "command", "computer", "secret", "org", "workergroup"},
	"slack":       {"session", "slackroute", "computer", "audit", "workergroup"},
	"slackroute":  {},
	"deployment":  {"agent", "computer", "slack", "slackroute", "session", "org", "audit"},
	"command":     {"audit", "computer", "secret", "workergroup", "org"},
	"identity":    {"org", "audit", "slack"},
	"org":         {"audit"},
	"secret":      {"audit"},
	"workergroup": {"audit"},
	"audit":       {},
}

func TestDurableOwnerDependencies(t *testing.T) {
	root := filepath.Join(repositoryRoot(t), "internal")
	graph, err := internalPackageDependencyGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	for owner, allowed := range ownerImports {
		for _, dependency := range graph[owner] {
			if _, isOwner := ownerImports[dependency]; isOwner && !slices.Contains(allowed, dependency) {
				t.Errorf("%s must not import owner %s", owner, dependency)
			}
		}
		files, err := os.ReadDir(filepath.Join(root, owner))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".go") || strings.HasSuffix(file.Name(), "_test.go") {
				continue
			}
			syntax, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, owner, file.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range syntax.Imports {
				path, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(path, moduleImportPrefix+"cmd/") || path == internalImportPrefix+"controlplane" {
					t.Errorf("durable owner %s/%s imports transport %s", owner, file.Name(), path)
				}
				if strings.HasPrefix(path, internalImportPrefix+"proto/") && (owner != "session" || !slices.Contains([]string{
					"computer_capture.go", "computer_capture_state.go", "computer_restore.go",
					"computer_source_abort.go", "computer_source_prepare.go", "computer_continuation_controls.go",
				}, file.Name())) {
					t.Errorf("durable owner %s/%s imports protobuf outside capture or continuation: %s", owner, file.Name(), path)
				}
			}
		}
	}
}
