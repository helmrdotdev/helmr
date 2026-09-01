package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateFinalizedDeploymentDefinitionsBuildsOneCanonicalBatch(t *testing.T) {
	artifactID := pgvalue.UUID(uuid.NewV7())
	creator := &recordingDeploymentDefinitionCreator{}
	definitions := []finalizedDeploymentDefinition{
		{kind: "task", declaredID: "task", manifest: []byte(`{"task":true}`), manifestDigest: []byte{1}},
		{
			kind: "sandbox", declaredID: "sandbox", manifest: []byte(`{"sandbox":true}`),
			manifestDigest: []byte{2}, artifact: &cas.Descriptor{Digest: "sha256:image"},
		},
	}
	if err := createFinalizedDeploymentDefinitions(
		t.Context(), creator, pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), definitions,
		map[string]db.Artifact{"sha256:image": {ID: artifactID}},
	); err != nil {
		t.Fatal(err)
	}
	if creator.calls != 1 {
		t.Fatalf("bulk calls = %d, want 1", creator.calls)
	}
	params := creator.params
	if len(params.Ids) != 2 || len(params.Kinds) != 2 || len(params.DeclaredIds) != 2 ||
		len(params.Manifests) != 2 || len(params.ManifestDigests) != 2 || len(params.ArtifactIds) != 2 {
		t.Fatalf("array cardinalities do not match: %+v", params)
	}
	firstID, firstErr := pgvalue.UUIDValue(params.Ids[0])
	secondID, secondErr := pgvalue.UUIDValue(params.Ids[1])
	if firstErr != nil || secondErr != nil || firstID == secondID {
		t.Fatalf("generated IDs = %v/%v errors=%v/%v", firstID, secondID, firstErr, secondErr)
	}
	if params.ArtifactIds[0].Valid || params.ArtifactIds[1] != artifactID {
		t.Fatalf("artifact IDs = %+v, want null then %v", params.ArtifactIds, artifactID)
	}
}

func TestCreateFinalizedDeploymentDefinitionsRejectsMissingArtifactMapping(t *testing.T) {
	creator := &recordingDeploymentDefinitionCreator{}
	err := createFinalizedDeploymentDefinitions(
		t.Context(), creator, pgtype.UUID{}, pgtype.UUID{},
		[]finalizedDeploymentDefinition{{
			kind: "sandbox", declaredID: "sandbox", artifact: &cas.Descriptor{Digest: "sha256:missing"},
		}},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), `artifact "sha256:missing" is not registered`) {
		t.Fatalf("error = %v", err)
	}
	if creator.calls != 0 {
		t.Fatalf("bulk calls = %d, want 0", creator.calls)
	}
}

func TestCreateFinalizedDeploymentDefinitionsRejectsResultCardinality(t *testing.T) {
	creator := &recordingDeploymentDefinitionCreator{inserted: 1, insertedSet: true}
	err := createFinalizedDeploymentDefinitions(
		t.Context(), creator, pgtype.UUID{}, pgtype.UUID{},
		[]finalizedDeploymentDefinition{{kind: "task"}, {kind: "actor"}}, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "inserted 1 of 2 rows") {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateFinalizedDeploymentDefinitionsAttributesDatabaseFailure(t *testing.T) {
	want := errors.New("database failure")
	creator := &recordingDeploymentDefinitionCreator{err: want}
	err := createFinalizedDeploymentDefinitions(
		t.Context(), creator, pgtype.UUID{}, pgtype.UUID{}, nil, nil,
	)
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "create deployment definition") {
		t.Fatalf("error = %v", err)
	}
}

type recordingDeploymentDefinitionCreator struct {
	params      db.CreateDeploymentDefinitionsParams
	inserted    int64
	insertedSet bool
	err         error
	calls       int
}

func (c *recordingDeploymentDefinitionCreator) CreateDeploymentDefinitions(
	_ context.Context,
	params db.CreateDeploymentDefinitionsParams,
) (int64, error) {
	c.calls++
	c.params = params
	if c.err != nil {
		return 0, c.err
	}
	if c.insertedSet {
		return c.inserted, nil
	}
	return int64(len(params.Ids)), nil
}
