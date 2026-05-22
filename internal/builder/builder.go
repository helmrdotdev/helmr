package builder

import (
	"context"
	"encoding/json"
	"time"

	bundlev0 "github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
)

type Engine interface {
	Build(context.Context, Request) (Artifact, error)
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

type Source struct {
	CheckoutRoot string
	ProjectRoot  string
	SHA          string
}

type Artifact struct {
	ImageTarPath string
	ConfigPath   string
	ManifestPath string
}
