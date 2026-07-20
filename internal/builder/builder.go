package builder

import (
	"context"
	"encoding/json"
	"time"

	"github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
)

type Engine interface {
	Build(context.Context, Request) (Artifact, error)
}

type ImageEngine interface {
	BuildImage(context.Context, ImageRequest) (Artifact, error)
}

type Request struct {
	RunID        string
	TaskID       string
	CacheScope   string
	Payload      json.RawMessage
	Bundle       *bundlev0.Bundle
	BuildSecrets map[string][]byte
	Source       Source
	MaxDuration  time.Duration
}

type ImageRequest struct {
	RunID       string
	WorkspaceID string
	CacheScope  string
	Build       ImageBuild
	Source      Source
}

type Source struct {
	CheckoutRoot string
	ProjectRoot  string
	SHA          string
}

type Artifact struct {
	RootPath     string
	ImageTarPath string
	ConfigPath   string
	ManifestPath string
}
