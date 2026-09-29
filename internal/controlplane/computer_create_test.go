package controlplane

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestValidateComputerKeyPreservesExactBytes(t *testing.T) {
	valid := " repository "
	if err := validateComputerKey(&valid); err == nil {
		t.Fatal("expected edge whitespace to be rejected without normalization")
	}
	exact := "répository"
	if err := validateComputerKey(&exact); err != nil {
		t.Fatalf("valid exact UTF-8 key: %v", err)
	}
}

func TestComputerPublicStatusUsesPublicSpelling(t *testing.T) {
	status, err := computerPublicStatus("recovery_required")
	if err != nil {
		t.Fatal(err)
	}
	if status != api.ComputerStatusAvailable || string(status) != "available" {
		t.Fatalf("recovery status = %q", status)
	}
}
