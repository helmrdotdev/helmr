package control

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDeploymentCreateConvergesAcrossTransactions(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	orgID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	regionID := "deployment-create-" + environmentID.String()
	mustDeploymentCreateExec(t, database.Pool, `
		INSERT INTO organizations (id, public_id, name, slug)
		VALUES ($1, $2, 'Deployment create', $3)
	`, orgID, deploymentCreatePublicID(t, publicid.Organization), "org-"+orgID.String())
	mustDeploymentCreateExec(t, database.Pool, `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'test', $1, 'Deployment create')
	`, regionID)
	mustDeploymentCreateExec(t, database.Pool, `
		INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, $5, 'Deployment create')
	`, projectID, deploymentCreatePublicID(t, publicid.Project), orgID, regionID, "project-"+projectID.String())
	mustDeploymentCreateExec(t, database.Pool, `
		INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'production', 'Production', '#000000')
	`, environmentID, deploymentCreatePublicID(t, publicid.Environment), orgID, projectID)

	source := api.DeploymentSourceArtifact{
		Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes: 4096,
		MediaType: api.DeploymentSourceArtifactMediaType,
	}
	selection := deployment.SourceSelection{
		NodeVersion: "24.16.0",
		Manager: deployment.PackageManager{
			Name:    deployment.PackageManagerPNPM,
			Version: "11.1.0",
		},
		LockfileName:   "pnpm-lock.yaml",
		LockfileDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	claimRequest, err := idempotency.NewDeploymentCreateRequest(
		environmentID,
		projectID,
		"deployment-create-1",
		idempotency.DeploymentCreateFingerprint{
			SourceDigest:         source.Digest,
			LockfileDigest:       selection.LockfileDigest,
			LockfileName:         selection.LockfileName,
			NodeVersion:          selection.NodeVersion,
			ManagerName:          string(selection.Manager.Name),
			ManagerVersion:       selection.Manager.Version,
			BuildContractVersion: deployment.ProgramBuildContractVersion,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db.New(database.Pool), tx: database.Pool}
	results := make(chan api.DeploymentResponse, 2)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var response api.DeploymentResponse
			err := server.inTx(context.Background(), func(work *txWork) error {
				store, ok := work.q.(deploymentStore)
				if !ok {
					return fmt.Errorf("deployment store is unavailable")
				}
				claims, err := idempotency.TransactionForQueries(work.q)
				if err != nil {
					return err
				}
				acquired, err := claims.Acquire(context.Background(), claimRequest)
				if err != nil {
					return err
				}
				if !acquired.New {
					response, err = replayDeploymentCreation(
						context.Background(),
						work.q,
						store,
						acquired.Claim,
						pgvalue.UUID(orgID),
						pgvalue.UUID(projectID),
					)
					return err
				}
				response, err = createDeploymentRecords(
					context.Background(),
					store,
					regionID,
					selection,
					orgID,
					pgvalue.UUID(projectID),
					pgvalue.UUID(environmentID),
					source.Digest,
					source,
					deploymentVersionMetadata{
						APIVersion:            api.CurrentAPIVersion,
						WorkerProtocolVersion: api.CurrentWorkerProtocolVersion,
					},
				)
				if err != nil {
					return err
				}
				return completeDeploymentCreation(
					context.Background(),
					claims,
					acquired.Claim,
					response.ID,
				)
			})
			if err != nil {
				errs <- err
				return
			}
			results <- response
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(results)
	var deploymentID string
	for response := range results {
		if deploymentID == "" {
			deploymentID = response.ID
			continue
		}
		if response.ID != deploymentID {
			t.Fatalf("concurrent responses differ: %s and %s", deploymentID, response.ID)
		}
	}
	var claimCount, deploymentCount, completedReceiptCount int
	if err := database.Pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM idempotency_claims
		      WHERE environment_id = $1
		        AND operation = 'deployment.create'
		        AND retired_at IS NULL),
		    (SELECT count(*) FROM deployments WHERE environment_id = $1),
		    (SELECT count(*) FROM idempotency_claims
		      WHERE environment_id = $1
		        AND operation = 'deployment.create'
		        AND state = 'completed'
		        AND receipt IS NOT NULL)
	`, environmentID).Scan(&claimCount, &deploymentCount, &completedReceiptCount); err != nil {
		t.Fatal(err)
	}
	if claimCount != 1 || deploymentCount != 1 || completedReceiptCount != 1 {
		t.Fatalf(
			"counts = claims %d deployments %d completed receipts %d",
			claimCount,
			deploymentCount,
			completedReceiptCount,
		)
	}
}

func mustDeploymentCreateExec(
	t *testing.T,
	executor interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	},
	query string,
	args ...any,
) {
	t.Helper()
	if _, err := executor.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func deploymentCreatePublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	value, err := publicid.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
