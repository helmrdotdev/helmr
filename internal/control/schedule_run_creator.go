package control

import (
	"errors"
	"log/slog"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

func NewScheduleRunCreator(log *slog.Logger, database dbTXBeginner, resolver githubCommitResolver, secrets secretManager, enqueuer runEnqueuer) (*Server, error) {
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
		github:      resolver,
		secrets:     secrets,
		runEnqueuer: enqueuer,
	}, nil
}
