package dispatch

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunStoreRejectsOversizedCandidateScopeBatch(t *testing.T) {
	params := db.ListQueuedRunDispatchCandidatesParams{
		OrgIds:            make([]pgtype.UUID, runDispatchCandidateScopeLimit+1),
		EnvironmentIds:    make([]pgtype.UUID, runDispatchCandidateScopeLimit+1),
		ConcurrencyKeys:   make([]string, runDispatchCandidateScopeLimit+1),
		QueueNames:        make([]string, runDispatchCandidateScopeLimit+1),
		CandidateLimits:   make([]int32, runDispatchCandidateScopeLimit+1),
		AfterSet:          make([]bool, runDispatchCandidateScopeLimit+1),
		AfterQueueScoreAt: make([]pgtype.Timestamptz, runDispatchCandidateScopeLimit+1),
		AfterRunIds:       make([]pgtype.UUID, runDispatchCandidateScopeLimit+1),
	}
	_, err := new(RunStore).ListCandidates(t.Context(), params)
	if err == nil || !strings.Contains(err.Error(), "scope count") {
		t.Fatalf("ListCandidates error = %v, want scope count error", err)
	}
}
