package controlplane

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

var errDeterministicWorkerAdmission = errors.New("worker admission is deterministically invalid")

func deterministicWorkerAdmission(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", errDeterministicWorkerAdmission, err)
}

func isDeterministicWorkerAdmission(err error) bool {
	if errors.Is(err, errDeterministicWorkerAdmission) {
		return true
	}
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23514"
}
