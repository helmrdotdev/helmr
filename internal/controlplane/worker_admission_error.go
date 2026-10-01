package controlplane

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// isDeterministicWorkerAdmission reports a worker admission that a database
// check constraint rejected.
func isDeterministicWorkerAdmission(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23514"
}
