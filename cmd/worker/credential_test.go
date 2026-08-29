package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestReadWorkerEnrollmentToken(t *testing.T) {
	token, err := workergroup.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker-token")
	if err := os.WriteFile(path, []byte(token.Raw), 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := readWorkerEnrollmentToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != token.Raw {
		t.Fatalf("token = %q", got)
	}
}

func TestReadWorkerEnrollmentTokenRejectsUnsafeFiles(t *testing.T) {
	token, err := workergroup.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid")
	if err := os.WriteFile(valid, []byte(token.Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Fatal(err)
	}
	wrongMode := filepath.Join(dir, "wrong-mode")
	if err := os.WriteFile(wrongMode, []byte(token.Raw), 0o644); err != nil {
		t.Fatal(err)
	}
	newline := filepath.Join(dir, "newline")
	if err := os.WriteFile(newline, []byte(token.Raw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", 129)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{symlink, wrongMode, newline, large} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := readWorkerEnrollmentToken(path); err == nil {
				t.Fatal("unsafe token file accepted")
			}
		})
	}
}
