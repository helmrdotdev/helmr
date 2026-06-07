package dispatch

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestRoundRobinQueueScopeSelectorInterleavesOrganizations(t *testing.T) {
	orgA := ids.ToPG(ids.New())
	orgB := ids.ToPG(ids.New())
	orgC := ids.ToPG(ids.New())
	input := []QueueScope{
		{OrgID: orgA, QueueName: "a-1"},
		{OrgID: orgA, QueueName: "a-2"},
		{OrgID: orgA, QueueName: "a-3"},
		{OrgID: orgB, QueueName: "b-1"},
		{OrgID: orgC, QueueName: "c-1"},
		{OrgID: orgC, QueueName: "c-2"},
	}

	got := RoundRobinQueueScopeSelector{}.Order(input)
	want := []QueueScope{
		{OrgID: orgA, QueueName: "a-1"},
		{OrgID: orgB, QueueName: "b-1"},
		{OrgID: orgC, QueueName: "c-1"},
		{OrgID: orgA, QueueName: "a-2"},
		{OrgID: orgC, QueueName: "c-2"},
		{OrgID: orgA, QueueName: "a-3"},
	}
	if !sameQueueScopes(got, want) {
		t.Fatalf("ordered scopes = %+v, want %+v", got, want)
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
