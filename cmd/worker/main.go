package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/workerhost"
)

func main() {
	if handled, err := runComputerHelper(os.Args); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("worker (build identity: signed release manifest and worker-host-artifacts.json)")
		return
	}
	if handled, err := verify.RunChild(os.Args); handled {
		if err != nil {
			_, _ = fmt.Fprintln(
				os.Stderr,
				"artifact verifier bootstrap failed:",
				verify.ChildLocalDiagnostic(err),
			)
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) == 1 {
		if err := run(log); err != nil {
			log.Error("worker stopped", "error", err)
			if errors.Is(err, workerhost.ErrKeyRejected) {
				// Service managers keep a stopped worker stopped on this
				// status instead of restarting it.
				os.Exit(workerhost.ExitKeyRejected)
			}
			os.Exit(1)
		}
		return
	}
	log.Error("the worker takes no arguments; manage it with helmr worker", "arguments", os.Args[1:])
	os.Exit(2)
}
