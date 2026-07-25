package dispatch

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNestedResumeCursorWrapsPastRejectedPrefixWithMovingTail(t *testing.T) {
	low := pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	high := pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000002"))
	tail := pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000003"))
	all := []nestedResumeCandidate{{runID: low}, {runID: high}}
	highWater := func(context.Context) (pgtype.UUID, error) {
		if len(all) == 0 {
			return pgtype.UUID{}, nil
		}
		return all[len(all)-1].runID, nil
	}
	list := func(
		_ context.Context,
		limit int32,
		after pgtype.UUID,
		sweepHigh pgtype.UUID,
	) ([]nestedResumeCandidate, error) {
		var candidates []nestedResumeCandidate
		for _, candidate := range all {
			if after.Valid &&
				pgvalue.UUIDString(candidate.runID) <= pgvalue.UUIDString(after) {
				continue
			}
			if sweepHigh.Valid &&
				pgvalue.UUIDString(candidate.runID) > pgvalue.UUIDString(sweepHigh) {
				continue
			}
			candidates = append(candidates, candidate)
			if len(candidates) == int(limit) {
				break
			}
		}
		return candidates, nil
	}

	var cursor nestedResumeCursor
	first, err := cursor.next(context.Background(), 1, highWater, list)
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, nestedResumeCandidate{runID: tail})
	second, err := cursor.next(context.Background(), 1, highWater, list)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := cursor.next(context.Background(), 1, highWater, list)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].runID != low {
		t.Fatalf("first page = %+v, want low invalid candidate", first)
	}
	if len(second) != 1 || second[0].runID != high {
		t.Fatalf("second page = %+v, want valid candidate after rejected prefix", second)
	}
	if len(wrapped) != 1 || wrapped[0].runID != low {
		t.Fatalf("wrapped page = %+v, want low candidate", wrapped)
	}
}
