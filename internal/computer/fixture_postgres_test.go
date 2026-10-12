package computer

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fixture is the seeded Environment and Computer the storage tests share.
type fixture struct {
	base                      agenttest.Fixture
	pool                      *pgxpool.Pool
	env, computer, deployment uuid.UUID
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := agenttest.New(t)
	return fixture{base: f, pool: f.Pool, env: f.Environment, computer: f.Computer, deployment: f.Deployment}
}

// requestedSave records a requested save of the fixture Computer's lease.
func (f fixture) requestedSave(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,1,(SELECT COALESCE(max(seq),0)+1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$3))`, f.env, id, f.computer)
	return id
}
