package schema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func Up(ctx context.Context, databaseURL string) error {
	return run(ctx, databaseURL, func(m *migrate.Migrate) error {
		return m.Up()
	})
}

func run(ctx context.Context, databaseURL string, apply func(*migrate.Migrate) error) error {
	source, err := iofs.New(FS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		_ = source.Close()
		return fmt.Errorf("open migration database: %w", err)
	}
	driver, err := pgxdriver.WithInstance(db, &pgxdriver.Config{})
	if err != nil {
		_ = source.Close()
		_ = db.Close()
		return fmt.Errorf("open migration driver: %w", err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		_ = source.Close()
		_ = db.Close()
		return fmt.Errorf("configure migrations: %w", err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			migrator.GracefulStop <- true
		case <-done:
		}
	}()
	err = apply(migrator)
	close(done)
	sourceErr, dbErr := migrator.Close()
	err = errors.Join(err, sourceErr, dbErr)
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(ctxErr, err)
	}
	return err
}
