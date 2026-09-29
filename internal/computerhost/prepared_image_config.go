package computerhost

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/oci"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func readPreparedImageConfig(ctx context.Context, path string, artifact workerapi.CASObject) (*computerv0.RuntimeImageConfig, error) {
	config, err := oci.ReadVerifiedConfig(ctx, path, artifact.Digest, artifact.SizeBytes)
	if err != nil {
		return nil, err
	}
	return &computerv0.RuntimeImageConfig{Env: config.Env, WorkingDir: config.WorkingDir, User: config.User, Entrypoint: config.Entrypoint, Cmd: config.Cmd}, nil
}
