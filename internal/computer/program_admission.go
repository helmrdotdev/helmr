package computer

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

// CanAdmitProgram reports whether the Computer's spec and pinned program
// admit a new member from deploymentID with specID. The caller holds the
// Computer lock through creation of the logical member; Instance assignment
// may follow later and must recheck the Instance barrier.
func CanAdmitProgram(ctx context.Context, q db.Querier, environmentID, computerID, specID, deploymentID pgtype.UUID) (bool, error) {
	admission, err := q.GetComputerProgramAdmission(ctx, db.GetComputerProgramAdmissionParams{
		EnvironmentID: environmentID, ComputerID: computerID,
		ComputerSpecID: specID, DeploymentID: deploymentID,
	})
	if err != nil {
		return false, fmt.Errorf("read computer program admission: %w", err)
	}
	if !admission.SpecCompatible || !admission.ProgramCompatible.Valid || !admission.ProgramCompatible.Bool {
		return false, nil
	}
	return true, nil
}
