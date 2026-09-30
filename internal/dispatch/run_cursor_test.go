package dispatch

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunDispatchCursorInterleavesOrganizationsAndScopes(t *testing.T) {
	organizations := []pgtype.UUID{dispatchTestUUID(1), dispatchTestUUID(2)}
	rows := []runDispatchScopeRow{
		{organizationOrdinal: 1, scope: testRunDispatchScope(organizations[0], 11, "a")},
		{organizationOrdinal: 2, scope: testRunDispatchScope(organizations[1], 21, "c")},
		{organizationOrdinal: 1, scope: testRunDispatchScope(organizations[0], 12, "b")},
		{organizationOrdinal: 2, scope: testRunDispatchScope(organizations[1], 22, "d")},
	}
	var cursor runDispatchCursor
	selectedOrganizations := cursor.chooseOrganizations(organizations, 2)
	selected, _ := cursor.chooseScopes(rows, selectedOrganizations, 4, 5)
	want := []string{"a", "c", "b", "d"}
	if len(selected) != len(want) {
		t.Fatalf("selected scope count = %d, want %d", len(selected), len(want))
	}
	for index := range want {
		if selected[index].queueName != want[index] {
			t.Fatalf("selected queues = %v, want %v", selected, want)
		}
	}
}

func TestRunDispatchCursorAdvancesAndWrapsCandidate(t *testing.T) {
	organizationID := dispatchTestUUID(1)
	scope := testRunDispatchScope(organizationID, 11, "a")
	row := db.ListQueuedRunDispatchCandidatesRow{
		OrgID: organizationID, RunID: dispatchTestUUID(2),
		QueueScoreAt: pgtype.Timestamptz{Valid: true},
	}
	var cursor runDispatchCursor
	cursor.advanceCandidate(scope, row, false)
	params := cursor.candidateParams([]runDispatchScope{scope}, []int32{3})
	if len(params.AfterSet) != 1 || !params.AfterSet[0] || params.AfterRunIds[0] != row.RunID {
		t.Fatalf("candidate params = %+v", params)
	}
	cursor.advanceCandidate(scope, row, true)
	params = cursor.candidateParams([]runDispatchScope{scope}, []int32{3})
	if params.AfterSet[0] {
		t.Fatalf("wrapped candidate params = %+v", params)
	}
}

func TestRunDispatchCursorWrapsWhenRemainingScopesDisappear(t *testing.T) {
	organizationID := dispatchTestUUID(1)
	scope := testRunDispatchScope(organizationID, 11, "a")
	var cursor runDispatchCursor
	state := cursor.organization(organizationID)
	state.after = runDispatchScopeCursor{
		environmentID: scope.environmentID, queueName: scope.queueName, set: true,
	}
	state.candidates[scope] = runDispatchCandidateCursor{set: true}
	selected, _ := cursor.chooseScopes(nil, []pgtype.UUID{organizationID}, 1, 2)
	if len(selected) != 0 || state.after.set || len(state.candidates) != 0 {
		t.Fatalf("cursor did not wrap after scopes disappeared: selected=%v state=%+v", selected, state)
	}
}

func TestRunLaneUsesUUIDRandomSuffix(t *testing.T) {
	tests := []struct {
		last byte
		want int16
	}{
		{last: 0, want: 0},
		{last: 63, want: 63},
		{last: 64, want: 0},
		{last: 255, want: 63},
	}
	for _, test := range tests {
		organizationID := dispatchTestUUID(test.last)
		if got := runLane(organizationID); got != test.want {
			t.Fatalf("run dispatch lane for suffix %d = %d, want %d", test.last, got, test.want)
		}
	}
}

func testRunDispatchScope(
	organizationID pgtype.UUID,
	environmentLastByte byte,
	queueName string,
) runDispatchScope {
	return runDispatchScope{
		orgID: organizationID, environmentID: dispatchTestUUID(environmentLastByte), queueName: queueName,
	}
}
