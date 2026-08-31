package db_test

import (
	"os"
	"strings"
	"testing"
)

func TestTokenTerminalQueriesPublishReconciliationIntentWithoutLockingRuns(t *testing.T) {
	body, err := os.ReadFile("query/tokens.sql")
	if err != nil {
		t.Fatal(err)
	}

	transitionSources := map[string]string{
		"CompleteToken":   "FROM completed",
		"CancelToken":     "FROM cancelled",
		"ExpireDueTokens": "FROM expired",
	}
	for name, transitionSource := range transitionSources {
		query := namedTokenQuery(t, string(body), name)
		for _, required := range []string{
			"INSERT INTO control_outbox",
			"'token.reconcile'",
			"'environmentId'",
			"'tokenId'",
			transitionSource,
		} {
			if !strings.Contains(query, required) {
				t.Fatalf("%s is missing %q:\n%s", name, required, query)
			}
		}
		if strings.Contains(query, "UPDATE run_waits") || strings.Contains(query, "UPDATE runs") {
			t.Fatalf("%s locks Run-owned authority from the Token transaction:\n%s", name, query)
		}
		if count := strings.Count(query, "INSERT INTO control_outbox"); count != 1 {
			t.Fatalf("%s publishes %d reconciliation intents, want one:\n%s", name, count, query)
		}
	}
}

func namedTokenQuery(t *testing.T, body string, name string) string {
	t.Helper()
	marker := "-- name: " + name + " "
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("token query %q is missing", name)
	}
	query := body[start:]
	if next := strings.Index(query[len(marker):], "-- name:"); next >= 0 {
		query = query[:len(marker)+next]
	}
	return query
}
