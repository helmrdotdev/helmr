package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/version"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version.String())
		return
	}
	if handled, err := deployment.RunVerifierChild(os.Args); handled {
		if err != nil {
			_, _ = fmt.Fprintln(
				os.Stderr,
				"artifact verifier bootstrap failed:",
				deployment.VerifierChildLocalDiagnostic(err),
			)
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) == 1 {
		if err := run(log); err != nil {
			log.Error("worker stopped", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "drain":
			if err := runDrain(log, os.Args[2:]); err != nil {
				log.Error("drain worker", "error", err)
				os.Exit(1)
			}
			return
		case "fence":
			if err := runFence(); err != nil {
				log.Error("fence worker", "error", err)
				os.Exit(1)
			}
			return
		case "status":
			if err := runStatus(log); err != nil {
				log.Error("get worker status", "error", err)
				os.Exit(1)
			}
			return
		default:
			log.Error("unknown command", "command", os.Args[1])
			os.Exit(1)
		}
	}
}
