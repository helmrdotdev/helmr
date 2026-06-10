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
