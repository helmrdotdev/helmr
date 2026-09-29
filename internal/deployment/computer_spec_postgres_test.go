package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestRegisterComputerSpecsPostgresReusedAcrossProgramDeployments(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	image := bundle.ComputerImageArtifact{
		Profile: definition.ComputerSeedProfile, Architecture: definition.ArchitectureX8664,
		Digest: "sha256:" + strings.Repeat("d", 64), SizeBytes: 4096, MediaType: definition.ComputerSeedMediaType,
	}
	manifest := definition.SandboxManifest{
		Image:     definition.SandboxImageManifest{Profile: image.Profile, Config: image.Config, ArtifactDigest: image.Digest, MediaType: image.MediaType},
		Resources: definition.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 512},
	}
	spec, err := definition.CompileComputerSpec(manifest, image.ComputerImage())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	canonical, manifestDigest, err := definition.CanonicalManifestAndDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	prepared := fixture.prepared
	prepared.objects = append(prepared.objects, spec.Seed)
	prepared.definitions = []preparedDefinition{{kind: "sandbox", declaredID: "environment", manifest: canonical, manifestDigest: manifestDigest[:], computerSpec: &spec}}
	queries := db.New(fixture.pool)
	for index := range 2 {
		prepared.root.Digest = "sha256:" + strings.Repeat(string(rune('e'+index)), 64)
		prepared.bundle.Program.Artifact.Digest = "sha256:" + strings.Repeat(string(rune('1'+index)), 64)
		prepared.objects[0].Digest = prepared.bundle.Program.Artifact.Digest
		prepared.definitions[0].declaredID = []string{"environment", "renamed-environment"}[index]
		tx, err := fixture.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = createDeployment(t.Context(), db.New(tx), pgvalue.UUID(fixture.orgID), fixture.projectID, fixture.environmentID, prepared)
		if err != nil {
			_ = tx.Rollback(t.Context())
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var specs, referenced, artifacts int
	if err := fixture.pool.QueryRow(t.Context(), `
	 SELECT (SELECT count(*) FROM computer_specs),
	        (SELECT count(DISTINCT computer_spec_id) FROM deployment_definitions),
	        (SELECT count(*) FROM artifacts WHERE kind='computer_image')`).Scan(&specs, &referenced, &artifacts); err != nil {
		t.Fatal(err)
	}
	if specs != 1 || referenced != 1 || artifacts != 1 {
		t.Fatalf("specs=%d referenced=%d image artifacts=%d", specs, referenced, artifacts)
	}
	var id uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `SELECT id FROM computer_specs`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	stored, err := queries.GetComputerSpec(t.Context(), db.GetComputerSpecParams{EnvironmentID: fixture.environmentID, ID: pgvalue.UUID(id)})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := definition.ParseComputerSpec(stored.Config, cas.Descriptor{Digest: stored.SeedDigest, SizeBytes: stored.SeedSizeBytes, MediaType: stored.SeedMediaType})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip.Config, spec.Config) || roundTrip.Digest != spec.Digest {
		t.Fatal("JSONB changed canonical spec identity")
	}
	params := db.RegisterComputerSpecParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: fixture.environmentID, Config: []byte(strings.Replace(string(spec.Config), `"milliCpu":1000`, `"milliCpu":2000`, 1)),
		Digest: spec.Digest[:], SeedArtifactID: stored.SeedArtifactID, SeedDigest: stored.SeedDigest, SeedSizeBytes: stored.SeedSizeBytes, SeedMediaType: stored.SeedMediaType,
	}
	if _, err := queries.RegisterComputerSpec(t.Context(), params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("same digest with different content = %v", err)
	}
	params.Config = spec.Config
	params.SeedSizeBytes++
	if _, err := queries.RegisterComputerSpec(t.Context(), params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("same digest with different descriptor = %v", err)
	}
	params.SeedSizeBytes = stored.SeedSizeBytes
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE computer_specs SET seed_artifact_id=NULL WHERE id=$1`, stored.ID); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	outcomes := make(chan error, 8)
	for range 8 {
		group.Go(func() {
			candidate := params
			candidate.ID = pgvalue.UUID(uuid.NewV7())
			row, err := queries.RegisterComputerSpec(t.Context(), candidate)
			if err == nil && (row.ID != stored.ID || row.SeedArtifactID != stored.SeedArtifactID) {
				err = errors.New("concurrent rematerialization changed immutable spec identity")
			}
			outcomes <- err
		})
	}
	group.Wait()
	close(outcomes)
	for err := range outcomes {
		if err != nil {
			t.Fatal(err)
		}
	}

}
