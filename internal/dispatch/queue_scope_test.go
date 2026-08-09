package dispatch

import (
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRoundRobinQueueScopeSelectorInterleavesOrganizations(t *testing.T) {
	orgA := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	orgB := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	orgC := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	input := []QueueScope{
		testQueueScope(orgA, "a-1"),
		testQueueScope(orgA, "a-2"),
		testQueueScope(orgA, "a-3"),
		testQueueScope(orgB, "b-1"),
		testQueueScope(orgC, "c-1"),
		testQueueScope(orgC, "c-2"),
	}

	got := RoundRobinQueueScopeSelector{}.Order(input)
	want := []QueueScope{
		input[0],
		input[3],
		input[4],
		input[1],
		input[5],
		input[2],
	}
	if !sameQueueScopes(got, want) {
		t.Fatalf("ordered scopes = %+v, want %+v", got, want)
	}
}

func TestRunCandidateParamsPreservesScopeOrder(t *testing.T) {
	scopes := []QueueScope{
		{
			OrgID:         pgtype.UUID{Bytes: [16]byte{15: 1}, Valid: true},
			ProjectID:     pgtype.UUID{Bytes: [16]byte{15: 2}, Valid: true},
			EnvironmentID: pgtype.UUID{Bytes: [16]byte{15: 3}, Valid: true},
			RegionID:      "us-east-1", ConcurrencyKey: "", QueueName: "first",
		},
		{
			OrgID:         pgtype.UUID{Bytes: [16]byte{15: 4}, Valid: true},
			ProjectID:     pgtype.UUID{Bytes: [16]byte{15: 5}, Valid: true},
			EnvironmentID: pgtype.UUID{Bytes: [16]byte{15: 6}, Valid: true},
			RegionID:      "eu-west-1", ConcurrencyKey: "serial", QueueName: "second",
		},
	}

	params, err := runCandidateParams(scopes, 17)
	if err != nil {
		t.Fatal(err)
	}
	if params.PerScopeLimit != 17 ||
		len(params.OrgIds) != 2 || len(params.ProjectIds) != 2 || len(params.EnvironmentIds) != 2 ||
		len(params.RegionIds) != 2 || len(params.ConcurrencyKeys) != 2 || len(params.QueueNames) != 2 ||
		params.OrgIds[1] != scopes[1].OrgID || params.ProjectIds[1] != scopes[1].ProjectID ||
		params.EnvironmentIds[1] != scopes[1].EnvironmentID || params.RegionIds[1] != scopes[1].RegionID ||
		params.ConcurrencyKeys[1] != scopes[1].ConcurrencyKey || params.QueueNames[1] != scopes[1].QueueName {
		t.Fatalf("run candidate params = %+v", params)
	}
}

func TestRunCandidateParamsRejectsInvalidScopeCount(t *testing.T) {
	if _, err := runCandidateParams(nil, 1); err == nil {
		t.Fatal("empty scope batch was accepted")
	}
	if _, err := runCandidateParams(make([]QueueScope, 33), 1); err == nil {
		t.Fatal("oversized scope batch was accepted")
	}
}

func sameQueueScopes(a, b []QueueScope) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func testQueueScope(orgID pgtype.UUID, queueName string) QueueScope {
	return QueueScope{OrgID: orgID, RegionID: "us-east-1", QueueName: queueName}
}
