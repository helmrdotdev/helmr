package control

import (
	"errors"
	"log/slog"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

func NewScheduleRunCreator(log *slog.Logger, database dbTXBeginner, secrets SecretManager, enqueuer RunEnqueuer) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if database == nil {
		return nil, errors.New("database is required")
	}
	queries := db.New(database)
	return &Server{
		log:         log,
		db:          queries,
		tx:          database,
		auth:        auth.NewDBAuthenticator(queries),
		secrets:     secrets,
		runEnqueuer: enqueuer,
	}, nil
}
