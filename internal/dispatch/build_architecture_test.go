package dispatch

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type buildArchitectureDiscovery struct {
	row db.ListQueuedDeploymentBuildCandidatesRow
}

func (f buildArchitectureDiscovery) ListQueuedDeploymentBuildRegions(context.Context, int32) ([]string, error) {
	return []string{f.row.BuildRegionID}, nil
}

func (f buildArchitectureDiscovery) ListQueuedDeploymentBuildCandidates(context.Context, db.ListQueuedDeploymentBuildCandidatesParams) ([]db.ListQueuedDeploymentBuildCandidatesRow, error) {
	return []db.ListQueuedDeploymentBuildCandidatesRow{f.row}, nil
}

type buildArchitectureAuthority struct {
	candidate ReadyBuildCandidate
}

func (f *buildArchitectureAuthority) PlaceReadyBuild(_ context.Context, candidate ReadyBuildCandidate, _ pgtype.Timestamptz) (db.LeaseQueuedDeploymentBuildRow, error) {
	f.candidate = candidate
	return db.LeaseQueuedDeploymentBuildRow{}, ErrCapacityUnavailable
}

func TestBuildPostgresFallbackCarriesDeploymentArchitecture(t *testing.T) {
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	deploymentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	discovery := buildArchitectureDiscovery{row: db.ListQueuedDeploymentBuildCandidatesRow{
		OrgID: orgID, DeploymentID: deploymentID, BuildRegionID: "us-east-1",
		BuildArchitecture: "aarch64", LeaseSequence: 1,
		BuildRequestedCpuMillis: 3000, BuildRequestedMemoryBytes: 4 << 30,
		BuildRequestedScratchBytes: 32 << 30, BuildRequestedExecutors: 1,
	}}
	authority := &buildArchitectureAuthority{}
	reconciler, err := NewPlacementReconciler(
		&countingRunPlacementDiscovery{}, isolationRunAuthority{},
		discovery, authority, isolationQueue{}, isolationWakePublisher{}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ReconcileBuilds(context.Background()); err != nil {
		t.Fatal(err)
	}
	if authority.candidate.BuildArchitecture != "aarch64" {
		t.Fatalf("fallback architecture = %q, want aarch64", authority.candidate.BuildArchitecture)
	}
	if authority.candidate.DeploymentID != deploymentID || authority.candidate.OrgID != orgID {
		t.Fatalf("fallback candidate identity = %+v", authority.candidate)
	}
}
