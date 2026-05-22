package main

import (
	"log/slog"
	"os"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "drain":
			if err := runDrain(log, os.Args[2:]); err != nil {
				log.Error("drain worker", "error", err)
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
	if err := run(log); err != nil {
		log.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}
