// Package computerdbtest inserts Computer rows directly into a test database:
// Computer specs, committed disk versions and their roots, and checkpoint
// artifacts. It sits beneath the Run and Session fixtures that compose them.
package computerdbtest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/jackc/pgx/v5"
)

func InsertDefaultComputerSpec(t *testing.T, ctx context.Context, executor interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, artifactID any) uuid.UUID {
	config := json.RawMessage(`{"architecture":"x86_64","image":{"Cmd":[],"Entrypoint":[],"Env":[],"User":"","WorkingDir":""},"profile":"linux-amd64-ext4-v1","resources":{"memoryMiB":512,"milliCpu":1000},"runtimeContract":"helmr.runtime.v0"}`)
	return InsertComputerSpec(t, ctx, executor, artifactID, config)
}

func InsertComputerSpec(t *testing.T, ctx context.Context, executor interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, artifactID any, config json.RawMessage) uuid.UUID {
	t.Helper()
	var environmentID uuid.UUID
	var seed cas.Descriptor
	if err := executor.QueryRow(ctx, `SELECT environment_id, digest, size_bytes, media_type FROM artifacts WHERE id=$1`, artifactID).
		Scan(&environmentID, &seed.Digest, &seed.SizeBytes, &seed.MediaType); err != nil {
		t.Fatal(err)
	}

	identity, err := json.Marshal(map[string]any{"config": config, "seed": map[string]any{"digest": seed.Digest, "sizeBytes": seed.SizeBytes, "mediaType": seed.MediaType}})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(identity)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(append([]byte("helmr.computer-spec.v0\x00"), canonical...))
	var id uuid.UUID
	if err := executor.QueryRow(ctx, `
		INSERT INTO computer_specs(id, environment_id, config, digest, seed_artifact_id, seed_digest, seed_size_bytes, seed_media_type)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT(environment_id,digest) DO UPDATE SET seed_artifact_id=COALESCE(computer_specs.seed_artifact_id,EXCLUDED.seed_artifact_id)
		RETURNING id`, uuid.NewV7(), environmentID, config, digest[:], artifactID, seed.Digest, seed.SizeBytes, seed.MediaType).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
