package schema

import (
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

type migrationSource interface {
	ReadUp(version uint) (io.ReadCloser, string, error)
	ReadDown(version uint) (io.ReadCloser, string, error)
}

func TestEmbeddedMigrations(t *testing.T) {
	source, err := iofs.New(FS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	version, err := source.First()
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("first migration version = %d", version)
	}
	currentVersion, err := CurrentVersion()
	if err != nil {
		t.Fatal(err)
	}
	assertMigrationPair(t, source, version)
	for {
		next, err := source.Next(version)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatalf("next migration after %d: %v", version, err)
		}
		assertMigrationPair(t, source, next)
		version = next
	}
	if _, err := source.Next(currentVersion); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("migration after current version %d exists or failed unexpectedly: %v", currentVersion, err)
	}
}

func assertMigrationPair(t *testing.T, source migrationSource, version uint) {
	t.Helper()
	reader, _, err := source.ReadUp(version)
	if err != nil {
		t.Fatalf("migration version %d does not have an up migration: %v", version, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close migration %d up reader: %v", version, err)
	}
	reader, _, err = source.ReadDown(version)
	if err != nil {
		t.Fatalf("migration version %d does not have a down migration: %v", version, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close migration %d down reader: %v", version, err)
	}
}
