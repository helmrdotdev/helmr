package guestd

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

func TestMain(main *testing.M) {
	root, err := os.MkdirTemp("", "helmr-guestd-test-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HELMR_GUESTD_TMPDIR", root); err != nil {
		panic(err)
	}
	code := main.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
