package token

import (
	"errors"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestValidateTokenWaitSessionCursor(t *testing.T) {
	sessionID := pgtype.UUID{Bytes: uuid.NewV7(), Valid: true}
	runID := uuid.NewV7()
	run := tokenWaitLockedRun{
		id: runID, sessionID: sessionID, entrypointKind: "actor",
	}
	for _, cursor := range []int64{4, 5} {
		err := validateTokenWaitSessionCursor(
			pgtype.Int8{Int64: cursor, Valid: true}, sessionID,
			pgtype.UUID{Bytes: runID, Valid: true}, 4, 6, run, "actor",
			pgtype.Int8{Int64: 3, Valid: true},
		)
		if err != nil {
			t.Fatalf("cursor %d rejected: %v", cursor, err)
		}
	}
	for _, cursor := range []pgtype.Int8{{}, {Int64: 3, Valid: true}, {Int64: 6, Valid: true}} {
		err := validateTokenWaitSessionCursor(
			cursor, sessionID, pgtype.UUID{Bytes: runID, Valid: true}, 4, 6,
			run, "actor", pgtype.Int8{Int64: 3, Valid: true},
		)
		if !errors.Is(err, ErrWaitAuthority) {
			t.Fatalf("cursor %+v error = %v, want authority error", cursor, err)
		}
	}
}

func TestValidateTokenWaitTaskRejectsSessionCursor(t *testing.T) {
	run := tokenWaitLockedRun{id: uuid.NewV7(), entrypointKind: "task"}
	if err := validateTokenWaitSessionCursor(
		pgtype.Int8{}, pgtype.UUID{}, pgtype.UUID{}, 0, 0, run, "task", pgtype.Int8{},
	); err != nil {
		t.Fatalf("Task NULL cursor rejected: %v", err)
	}
	if err := validateTokenWaitSessionCursor(
		pgtype.Int8{Int64: 0, Valid: true}, pgtype.UUID{}, pgtype.UUID{}, 0, 0,
		run, "task", pgtype.Int8{},
	); !errors.Is(err, ErrWaitAuthority) {
		t.Fatalf("Task Actor cursor error = %v, want authority error", err)
	}
}
