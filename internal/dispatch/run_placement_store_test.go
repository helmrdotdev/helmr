package dispatch

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunPlacementStoreRejectsOversizedCandidateScopeBatch(t *testing.T) {
	params := db.ListQueuedRunPlacementCandidatesParams{
		OrgIds:            make([]pgtype.UUID, runPlacementCandidateScopeLimit+1),
		EnvironmentIds:    make([]pgtype.UUID, runPlacementCandidateScopeLimit+1),
		ConcurrencyKeys:   make([]string, runPlacementCandidateScopeLimit+1),
		QueueNames:        make([]string, runPlacementCandidateScopeLimit+1),
		CandidateLimits:   make([]int32, runPlacementCandidateScopeLimit+1),
		AfterSet:          make([]bool, runPlacementCandidateScopeLimit+1),
		AfterQueueScoreAt: make([]pgtype.Timestamptz, runPlacementCandidateScopeLimit+1),
		AfterRunIds:       make([]pgtype.UUID, runPlacementCandidateScopeLimit+1),
	}
	_, err := new(RunPlacementStore).ListCandidates(t.Context(), params)
	if err == nil || !strings.Contains(err.Error(), "scope count") {
		t.Fatalf("ListCandidates error = %v, want scope count error", err)
	}
}
