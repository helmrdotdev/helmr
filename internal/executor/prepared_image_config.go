package executor

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/oci"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func readPreparedImageConfig(ctx context.Context, path string, artifact workerapi.CASObject) (*workspacev0.RuntimeImageConfig, error) {
	config, err := oci.ReadVerifiedConfig(ctx, path, artifact.Digest, artifact.SizeBytes)
	if err != nil {
		return nil, err
	}
	return &workspacev0.RuntimeImageConfig{Env: config.Env, WorkingDir: config.WorkingDir, User: config.User, Entrypoint: config.Entrypoint, Cmd: config.Cmd}, nil
}
