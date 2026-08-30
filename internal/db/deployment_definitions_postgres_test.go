package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func TestDeploymentDefinitionManifestJSONBRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	fixture := loadDefinitionContractFixture(t)

	canonical, digest, err := deployment.CanonicalManifestAndDigest([]byte(fixture.Manifest.Input))
	if err != nil {
		t.Fatal(err)
	}
	definitionID := uuid.NewV7()
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest
		) VALUES ($1, $2, $3, 'task', 'canonical-roundtrip', 0, $4::jsonb, $5)
	`, definitionID, ids.environmentID, ids.deploymentID, canonical, digest[:]); err != nil {
		t.Fatal(err)
	}

	var storedJSON []byte
	var storedDigest []byte
	if err := pool.QueryRow(ctx, `
		SELECT manifest::text, manifest_digest
		  FROM deployment_definitions
		 WHERE id = $1
	`, definitionID).Scan(&storedJSON, &storedDigest); err != nil {
		t.Fatal(err)
	}
	recanonical, redigest, err := deployment.CanonicalManifestAndDigest(storedJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recanonical, canonical) {
		t.Fatalf("recanonical manifest = %s, want %s", recanonical, canonical)
	}
	if !bytes.Equal(storedDigest, digest[:]) || !bytes.Equal(redigest[:], digest[:]) {
		t.Fatalf("stored/recanonical digest = %x/%x, want %x", storedDigest, redigest, digest)
	}
}

type definitionContractFixture struct {
	Manifest struct {
		Input string `json:"input"`
	} `json:"manifest"`
}

func loadDefinitionContractFixture(t *testing.T) definitionContractFixture {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "fixtures", "contracts", "deployment-v0", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture definitionContractFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}
