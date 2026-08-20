package controlplane

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDeterministicWorkerAdmissionClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "marked validation", err: deterministicWorkerAdmission(errors.New("invalid pinned policy")), want: true},
		{name: "wrapped check violation", err: fmt.Errorf("complete task: %w", &pgconn.PgError{Code: "23514"}), want: true},
		{name: "unique violation", err: &pgconn.PgError{Code: "23505"}},
		{name: "transient database failure", err: errors.New("connection reset")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isDeterministicWorkerAdmission(test.err); got != test.want {
				t.Fatalf("isDeterministicWorkerAdmission() = %v, want %v", got, test.want)
			}
		})
	}
}
