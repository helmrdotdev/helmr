package control

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func parseWorkspacePrimitiveLimit(r *http.Request, defaultLimit int32, maxLimit int32) (int32, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return defaultLimit, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value <= 0 {
		return 0, errors.New("limit must be a positive integer")
	}
	if value > int64(maxLimit) {
		return 0, fmt.Errorf("limit must be at most %d", maxLimit)
	}
	return int32(value), nil
}

func parseWorkspaceStreamCursor(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if raw == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cursor < 0 {
		return 0, errors.New("cursor must be a non-negative integer")
	}
	return cursor, nil
}

func actorSubjectID(actor auth.Actor) string {
	switch actor.Kind {
	case auth.ActorKindAPIKey:
		if actor.APIKeyID != uuid.Nil {
			return actor.APIKeyID.String()
		}
	case auth.ActorKindSession:
		if actor.SessionID != uuid.Nil {
			return actor.SessionID.String()
		}
	case auth.ActorKindSystem:
		if actor.UserID != uuid.Nil {
			return actor.UserID.String()
		}
	}
	if actor.UserID != uuid.Nil {
		return actor.UserID.String()
	}
	return ""
}

func isExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}

func (s *Server) writeWorkspacePrimitiveError(w http.ResponseWriter, op string, err error) {
	var apiErr apiError
	if errors.As(err, &apiErr) {
		writeError(w, err)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("workspace primitive not found")))
		return
	}
	if errors.Is(err, errWorkspaceStreamOffsetConflict) {
		writeError(w, conflict(errWorkspaceStreamOffsetConflict))
		return
	}
	if errors.Is(err, errWorkspaceStreamCursorExpired) {
		writeError(w, gone(errWorkspaceStreamCursorExpired))
		return
	}
	if errors.Is(err, errWorkspaceReadOnlyUnsupported) {
		writeError(w, badRequest(errWorkspaceReadOnlyUnsupported))
		return
	}
	writeError(w, fmt.Errorf("%s: %w", op, err))
}
