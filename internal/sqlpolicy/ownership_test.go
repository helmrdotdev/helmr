package sqlpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Data-access rule: every statement a production package sends to PostgreSQL
// is a compile-time constant at its call site (so TestStatementsPrepareAgainstSchema
// parses it against the fresh schema), or a documented exception below; and
// every table has one owning package that writes it. Generated sqlc queries
// belong to the owner that calls them. Reads cross owners freely; the owner
// split narrows writers further.

// dynamicStatementSites are the call sites whose statement is not a constant,
// keyed as package:declaration, each with the reason the rule tolerates it.
var dynamicStatementSites = map[string]string{
	"cmd/control-plane:bootstrapDatabase":           "runs the checked-in migration files",
	"cmd/internal/dev-controlplane:migrate":         "runs the checked-in migration files",
	"internal/computer:reclaimComputerGraphRow":     "statement is a constant at each caller, which the preparation test covers",
	"internal/slackroute:lockConnections":           "two constant lock statements selected by lock strength",
	"internal/telemetry:BeginDiagnosticAdmission":   "owner table and prefix composed over a fixed set; covered by telemetry PostgreSQL tests",
	"internal/telemetry:DiagnosticAdmission.Append": "owner table and prefix composed over a fixed set; covered by telemetry PostgreSQL tests",
}

// tableOwners maps each written table to the package that owns its writes.
// Owner names are import paths without the module prefix.
var tableOwners = map[string]string{
	// identity
	"api_keys": "internal/identity", "auth_identities": "internal/identity", "login_sessions": "internal/identity",
	"device_codes": "internal/identity", "magic_links": "internal/identity", "users": "internal/identity",
	// organizations, projects, environments
	"environments": "internal/org", "invitations": "internal/org", "org_members": "internal/org",
	"organizations": "internal/org", "projects": "internal/org",
	"regions": "internal/region",
	// deployments and definitions
	"agent_definitions": "internal/deployment", "agent_triggers": "internal/deployment", "agents": "internal/deployment",
	"trigger_activations": "internal/agent",
	"image_specs":         "internal/computer", "deployment_objects": "internal/deployment", "deployments": "internal/deployment",
	"template_definitions": "internal/deployment", "templates": "internal/deployment",
	// Computers, their allocations, images and storage
	"cas_blobs": "internal/computer", "cas_objects": "internal/computer", "cas_upload_reclaims": "internal/computer",
	"checkpoint_processes": "internal/computer", "checkpoint_objects": "internal/computer", "checkpoints": "internal/computer",
	"disk_encryption_keys": "internal/computer", "disk_roots": "internal/computer", "images": "internal/computer",
	"computer_leases": "internal/computer", "disk_object_edges": "internal/computer", "disk_object_keys": "internal/computer",
	"disk_object_pins": "internal/computer", "disk_objects": "internal/computer", "image_builds": "internal/computer",
	"computer_saves": "internal/computer", "secret_bindings": "internal/computer", "computers": "internal/computer",
	"secret_exposures": "internal/computer",
	// Sessions and Turns
	"trigger_occurrences": "internal/agent", "session_controls": "internal/session",
	"session_events": "internal/session", "session_holds": "internal/session", "session_processes": "internal/session",
	"sessions": "internal/session", "turn_asks": "internal/session", "turn_messages": "internal/session", "turns": "internal/session",
	// commands
	"commands": "internal/command", "idempotency_receipts": "internal/idempotency",
	// secrets
	"secrets": "internal/secret", "secret_versions": "internal/secret",
	// Slack
	"slack_app_registrations": "internal/slack", "slack_installations": "internal/slack", "slack_posts": "internal/slack",
	"slack_requests": "internal/slack", "slack_threads": "internal/slack", "slack_user_links": "internal/slack",
	// Slack route authority
	"slack_connections": "internal/slackroute", "slack_channel_bindings": "internal/slackroute", "slack_thread_sources": "internal/slackroute",
	// telemetry
	"diagnostic_chunks": "internal/telemetry", "deployment_events": "internal/deployment",
	// worker supply
	"vm_platforms": "internal/workergroup", "worker_group_tokens": "internal/workergroup", "worker_groups": "internal/workergroup",
	"worker_host_secrets": "internal/workergroup", "worker_hosts": "internal/workergroup", "worker_pool_cpu_shapes": "internal/workergroup",
	"worker_pools":      "internal/workergroup",
	"schema_migrations": "cmd/internal/dev-controlplane",
}

// coWriters are packages that still write another owner's table. Each entry
// names the slice that retires it; the owner split (B1) is the default.
var coWriters = map[string]map[string]string{
	"environments":   {"internal/deployment": "owner split"},
	"slack_posts":    {"internal/session": "owner split"},
	"slack_requests": {"internal/session": "owner split"},
	"login_sessions": {"internal/org": "member disablement revokes sessions"},
	"users":          {"internal/org": "organization creation records the owner"},
}

// queryFileOwners maps each sqlc query file to the owner whose writes it
// performs; a file consumed by several packages is owned by the writer.
var queryFileOwners = map[string]string{
	"allocation_planning.sql":   "internal/workergroup",
	"api_keys.sql":              "internal/identity",
	"auth_identities.sql":       "internal/identity",
	"login_sessions.sql":        "internal/identity",
	"cas_objects.sql":           "internal/computer",
	"cas_reclamation.sql":       "internal/computer",
	"commands.sql":              "internal/command",
	"disk_roots.sql":            "internal/computer",
	"disk_objects.sql":          "internal/computer",
	"device_codes.sql":          "internal/identity",
	"invitations.sql":           "internal/org",
	"magic_links.sql":           "internal/identity",
	"org_members.sql":           "internal/org",
	"organizations.sql":         "internal/org",
	"idempotency_receipts.sql":  "internal/idempotency",
	"projects.sql":              "internal/org",
	"regions.sql":               "internal/region",
	"trigger_reads.sql":         "internal/controlplane",
	"secrets.sql":               "internal/secret",
	"worker_capacity_hosts.sql": "internal/workergroup",
	"worker_drain.sql":          "internal/workergroup",
	"worker_groups.sql":         "internal/workergroup",
	"worker_host_secrets.sql":   "internal/workergroup",
	"worker_lifecycle.sql":      "internal/workergroup",
	"worker_liveness.sql":       "internal/workergroup",
	"worker_startup.sql":        "internal/workergroup",
}

// seedWriters write arbitrary rows outside ownership: development seeds.
var seedWriters = map[string]bool{"cmd/internal/dev-controlplane": true}

func allowedWriter(table, pkg string) bool {
	if seedWriters[pkg] {
		return true
	}
	if owner, ok := tableOwners[table]; ok && owner == pkg {
		return true
	}
	_, ok := coWriters[table][pkg]
	return ok
}

// TestStatementsAreConstantOrDocumented fails on a statement the preparation
// test cannot see unless its site carries a reason, and on stale reasons.
func TestStatementsAreConstantOrDocumented(t *testing.T) {
	matched := map[string]bool{}
	for _, s := range collectStatements(t) {
		if s.sql != "" {
			continue
		}
		if _, ok := dynamicStatementSites[s.site()]; ok {
			matched[s.site()] = true
			continue
		}
		t.Errorf("%s: %s sends a statement that is not a compile-time constant", s.position, s.site())
	}
	for site := range dynamicStatementSites {
		if !matched[site] {
			t.Errorf("exception %s matches no dynamic statement", site)
		}
	}
}

// TestTableWritesStayWithOwners checks handwritten and generated writes
// against the ownership table and keeps the co-writer list exact.
func TestTableWritesStayWithOwners(t *testing.T) {
	used := map[string]map[string]bool{}
	note := func(table, pkg string) {
		if used[table] == nil {
			used[table] = map[string]bool{}
		}
		used[table][pkg] = true
	}
	for _, s := range collectStatements(t) {
		for _, table := range writtenTables(s.sql) {
			note(table, s.pkg)
			if !allowedWriter(table, s.pkg) {
				t.Errorf("%s: %s writes %s owned by %q", s.position, s.pkg, table, tableOwners[table])
			}
		}
	}
	root := repositoryRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "internal", "db", "query"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		owner, ok := queryFileOwners[entry.Name()]
		if !ok {
			t.Errorf("query file %s has no owner", entry.Name())
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, "internal", "db", "query", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range writtenTables(string(content)) {
			note(table, owner)
			if !allowedWriter(table, owner) {
				t.Errorf("query/%s (%s) writes %s owned by %q", entry.Name(), owner, table, tableOwners[table])
			}
		}
	}
	for file := range queryFileOwners {
		if _, err := os.Stat(filepath.Join(root, "internal", "db", "query", file)); err != nil {
			t.Errorf("query file owner entry %s has no file", file)
		}
	}
	for table, writers := range coWriters {
		for pkg := range writers {
			if !used[table][pkg] {
				t.Errorf("co-writer %s of %s no longer writes it", pkg, table)
			}
		}
	}
	for table := range tableOwners {
		if len(used[table]) == 0 {
			t.Errorf("owned table %s is written by no statement", table)
		}
	}
}

var sqlcTableName = regexp.MustCompile(`(?m)^CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)`)

// TestEveryTableHasAnOwner keeps the ownership table complete against the schema.
func TestEveryTableHasAnOwner(t *testing.T) {
	root := repositoryRoot(t)
	entries, err := filepath.Glob(filepath.Join(root, "internal", "db", "schema", "migrations", "*.up.sql"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("migrations: %v %v", entries, err)
	}
	var missing []string
	for _, entry := range entries {
		content, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range sqlcTableName.FindAllStringSubmatch(string(content), -1) {
			if _, ok := tableOwners[match[1]]; !ok {
				missing = append(missing, match[1])
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("tables without an owner: %s", strings.Join(missing, ", "))
	}
	for table := range tableOwners {
		if table == "schema_migrations" {
			continue
		}
		found := false
		for _, entry := range entries {
			content, _ := os.ReadFile(entry)
			if strings.Contains(string(content), fmt.Sprintf("CREATE TABLE %s", table)) || strings.Contains(string(content), fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s", table)) {
				found = true
			}
		}
		if !found {
			t.Errorf("owned table %s is not in the schema", table)
		}
	}
}
