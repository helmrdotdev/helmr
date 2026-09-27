package controlplane

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

// The caller holds the Computer lock through creation of the logical member.
// Physical placement may follow later; it must recheck the instance barrier.
func computerCanAdmitProgram(ctx context.Context, q db.Querier, environmentID, computerID, specID, deploymentID pgtype.UUID) (bool, error) {
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
