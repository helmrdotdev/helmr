package dispatch

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunPlacementCursorInterleavesOrganizationsAndScopes(t *testing.T) {
	organizations := []pgtype.UUID{placementTestUUID(1), placementTestUUID(2)}
	rows := []runPlacementScopeRow{
		{organizationOrdinal: 1, scope: testRunPlacementScope(organizations[0], 11, "a")},
		{organizationOrdinal: 2, scope: testRunPlacementScope(organizations[1], 21, "c")},
		{organizationOrdinal: 1, scope: testRunPlacementScope(organizations[0], 12, "b")},
		{organizationOrdinal: 2, scope: testRunPlacementScope(organizations[1], 22, "d")},
	}
	var cursor runPlacementCursor
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

func TestRunPlacementCursorAdvancesAndWrapsCandidate(t *testing.T) {
	organizationID := placementTestUUID(1)
	scope := testRunPlacementScope(organizationID, 11, "a")
	row := db.ListQueuedRunPlacementCandidatesRow{
		OrgID: organizationID, RunID: placementTestUUID(2),
		QueueScoreAt: pgtype.Timestamptz{Valid: true},
	}
	var cursor runPlacementCursor
	cursor.advanceCandidate(scope, row, false)
	params := cursor.candidateParams([]runPlacementScope{scope}, []int32{3})
	if len(params.AfterSet) != 1 || !params.AfterSet[0] || params.AfterRunIds[0] != row.RunID {
		t.Fatalf("candidate params = %+v", params)
	}
	cursor.advanceCandidate(scope, row, true)
	params = cursor.candidateParams([]runPlacementScope{scope}, []int32{3})
	if params.AfterSet[0] {
		t.Fatalf("wrapped candidate params = %+v", params)
	}
}

func TestRunPlacementCursorWrapsWhenRemainingScopesDisappear(t *testing.T) {
	organizationID := placementTestUUID(1)
	scope := testRunPlacementScope(organizationID, 11, "a")
	var cursor runPlacementCursor
	state := cursor.organization(organizationID)
	state.after = runPlacementScopeCursor{
		environmentID: scope.environmentID, queueName: scope.queueName, set: true,
	}
	state.candidates[scope] = runPlacementCandidateCursor{set: true}
	selected, _ := cursor.chooseScopes(nil, []pgtype.UUID{organizationID}, 1, 2)
	if len(selected) != 0 || state.after.set || len(state.candidates) != 0 {
		t.Fatalf("cursor did not wrap after scopes disappeared: selected=%v state=%+v", selected, state)
	}
}

func TestRunPlacementLaneUsesUUIDRandomSuffix(t *testing.T) {
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
		organizationID := placementTestUUID(test.last)
		if got := runPlacementLane(organizationID); got != test.want {
			t.Fatalf("run placement lane for suffix %d = %d, want %d", test.last, got, test.want)
		}
	}
}

func testRunPlacementScope(
	organizationID pgtype.UUID,
	environmentLastByte byte,
	queueName string,
) runPlacementScope {
	return runPlacementScope{
		orgID: organizationID, environmentID: placementTestUUID(environmentLastByte), queueName: queueName,
	}
}
