package schema

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sync"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var FS embed.FS

var (
	currentVersionOnce sync.Once
	currentVersion     uint
	currentVersionErr  error
)

// CurrentVersion returns the highest embedded migration version.
func CurrentVersion() (uint, error) {
	currentVersionOnce.Do(func() {
		currentVersion, currentVersionErr = readCurrentVersion()
	})
	return currentVersion, currentVersionErr
}

func readCurrentVersion() (uint, error) {
	source, err := iofs.New(FS, "migrations")
	if err != nil {
		return 0, fmt.Errorf("open embedded migrations: %w", err)
	}
	defer source.Close()

	version, err := source.First()
	if err != nil {
		return 0, fmt.Errorf("read first migration version: %w", err)
	}
	for {
		next, err := source.Next(version)
		if errors.Is(err, fs.ErrNotExist) {
			return version, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read migration version after %d: %w", version, err)
		}
		version = next
	}
}
