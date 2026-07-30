package imagebuild

import (
	"context"
)

type Engine interface {
	BuildImage(context.Context, Request) (Artifact, error)
}

type Request struct {
	RunID       string
	WorkspaceID string
	Build       Build
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
