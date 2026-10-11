package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
)

func TestRevocationDerivedQueriesPreserveViewPlans(t *testing.T) {
	f, e, store, secretID, attachment, _ := runtimeSecretsFixture(t)
	if _, err := RecordSessionStartExposure(t.Context(), f.pool, *f.host(), e, attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(t.Context(), f.env, secretID, "plan-revoke"); err != nil {
		t.Fatal(err)
	}
	initial, err := schema.FS.ReadFile("migrations/000001_initial.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, view, ok := strings.Cut(string(initial), "CREATE VIEW computer_secret_revocations AS\n")
	if !ok {
		t.Fatal("missing original view")
	}
	view, _, ok = strings.Cut(view, "\n;")
	if !ok {
		t.Fatal("missing original view terminator")
	}
	dbtest.MustExec(t, t.Context(), f.pool, "CREATE VIEW computer_secret_revocations AS "+view)
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{"SELECT EXISTS(SELECT 1 FROM %s revoked WHERE revoked.environment_id=$1 AND revoked.computer_id=$2)", []any{f.env, f.computer}},
		{"SELECT DISTINCT revoked.environment_id,revoked.computer_id FROM %s revoked JOIN session_processes p ON (p.environment_id,p.computer_id)=(revoked.environment_id,revoked.computer_id) WHERE p.fenced_at IS NULL AND p.status IN ('starting','ready')", nil},
	} {
		plans := make([]any, 0, 2)
		for _, relation := range []string{"computer_secret_revocations", "(" + db.RevokedComputerSecretsSQL + ")"} {
			var raw []byte
			sql := "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) " + strings.Replace(query.sql, "%s", relation, 1)
			if err := f.pool.QueryRow(t.Context(), sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var explained []map[string]any
			if err := json.Unmarshal(raw, &explained); err != nil {
				t.Fatal(err)
			}
			t.Logf("query=%s relation=%s planning_ms=%v execution_ms=%v", query.sql, relation[:1], explained[0]["Planning Time"], explained[0]["Execution Time"])
			plan := explained[0]["Plan"]
			stripPlanMeasurements(plan)
			plans = append(plans, plan)
		}
		if !reflect.DeepEqual(plans[0], plans[1]) {
			t.Fatalf("derived relation changed query plan:\nview=%v\nderived=%v", plans[0], plans[1])
		}
	}
}

// Wall-clock times and buffer warmth vary between equivalent executions. Keep
// planner structure, costs, filters and index choices in the comparison.
func stripPlanMeasurements(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.HasPrefix(key, "Actual ") || strings.Contains(key, " Blocks") || strings.HasPrefix(key, "I/O ") || strings.HasPrefix(key, "Rows Removed") || key == "Heap Fetches" {
				delete(typed, key)
				continue
			}
			stripPlanMeasurements(child)
		}
	case []any:
		for _, child := range typed {
			stripPlanMeasurements(child)
		}
	}
}
