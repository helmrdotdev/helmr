package control

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

func NewScheduleRunCreator(log *slog.Logger, database dbTXBeginner, secrets SecretManager, enqueuer RunEnqueuer, eventStream *EventStream) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if database == nil {
		return nil, errors.New("database is required")
	}
	if eventStream == nil || eventStream.redis == nil {
		return nil, errors.New("event stream is required")
	}
	cellID := strings.TrimSpace(eventStream.cellID)
	if cellID == "" {
		return nil, errors.New("event stream cell id is required")
	}
	queries := db.New(database)
	return &Server{
		log:         log,
		cellID:      cellID,
		db:          queries,
		tx:          database,
		auth:        auth.NewDBAuthenticator(queries),
		secrets:     secrets,
		runEnqueuer: enqueuer,
		eventStream: eventStream,
	}, nil
}
