package db_test

import (
	"bytes"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
)

func TestPreparationSpecJSONBRoundTrip(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	spec := artifact.ComputerPreparationSpec{PrepareNetworkPolicy: networkpolicy.Policy{Kind: networkpolicy.Allowlist, Domains: []networkpolicy.Domain{{Domain: "registry.example.com"}}}, APIVersion: artifact.ComputerPreparationSpecVersion, TemplateDefinitionID: "repo", Program: artifact.ProgramDescriptor{Digest: dbtest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}}
	canonical, err := artifact.CanonicalComputerPreparationSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := artifact.ComputerPreparationSpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewV7()
	dbtest.MustExec(t, ctx, pool, `INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed) VALUES($1,$2,$3,$4::jsonb,'{}')`, ids.environmentID, id, digest, canonical)
	var raw []byte
	var storedDigest string
	if err := pool.QueryRow(ctx, `SELECT spec::text,spec_digest FROM image_specs WHERE environment_id=$1 AND id=$2`, ids.environmentID, id).Scan(&raw, &storedDigest); err != nil {
		t.Fatal(err)
	}
	parsed, err := artifact.ParseComputerPreparationSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	recanonical, err := artifact.CanonicalComputerPreparationSpec(parsed)
	if err != nil {
		t.Fatal(err)
	}
	redigest, err := artifact.ComputerPreparationSpecDigest(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, recanonical) || digest != storedDigest || digest != redigest {
		t.Fatalf("JSONB changed preparation identity: %+v %q %q", parsed, storedDigest, redigest)
	}
}
