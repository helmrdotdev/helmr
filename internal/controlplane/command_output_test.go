package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"testing"
	"time"
)

func TestCommandOutputClosureRequiresPipeEvidence(t *testing.T) {
	c := db.ComputerCommand{ComputerLeaseEpoch: pgtype.Int8{Int64: 1, Valid: true}, TerminalAt: pgvalue.Timestamptz(time.Now()), TerminalReasonCode: pgvalue.Text("computer_command_completed"), StdoutFinalThrough: pgtype.Int8{Int64: 2, Valid: true}, StderrFinalThrough: pgtype.Int8{Int64: 1, Valid: true}}
	if got := commandOutputState(c); got != "open" {
		t.Fatal(got)
	}
	c.StdoutAcceptedThrough, c.StderrAcceptedThrough = 2, 1
	c.StdoutEnded, c.StderrEnded = true, true
	c.StdoutEndComplete, c.StderrEndComplete = true, true
	if got := commandOutputState(c); got != "closed" {
		t.Fatal(got)
	}
	c.StdoutEndComplete = false
	if got := commandOutputState(c); got != "unavailable" {
		t.Fatal(got)
	}
	c.StdoutEnded, c.StderrEnded = false, false
	c.OutputFenced = true
	if got := commandOutputState(c); got != "unavailable" {
		t.Fatal(got)
	}
	c.OutputFenced = false
	c.Status = "lost"
	if got := commandOutputState(c); got != "unavailable" {
		t.Fatal(got)
	}
	c.ComputerLeaseEpoch = pgtype.Int8{}
	if got := commandOutputState(c); got != "closed" {
		t.Fatal(got)
	}
}
