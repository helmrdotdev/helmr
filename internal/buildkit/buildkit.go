package buildkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/builder"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/grpcerrors"
	"google.golang.org/grpc/codes"
)

const (
	defaultBuildKitAddr = "unix:///run/helmr/buildkit/buildkitd.sock"
	defaultOutputRoot   = "helmr-worker-builds"
	defaultPlatform     = "linux/amd64"
	defaultCacheNS      = "helmr"
	buildKitService     = "helmr-buildkit.service"
)

type Config struct {
	Addr           string
	OutputRoot     string
	CacheNamespace string
}

func (cfg Config) addr() string {
	if addr := strings.TrimSpace(cfg.Addr); addr != "" {
		return addr
	}
	return defaultBuildKitAddr
}

func (cfg Config) endpoint() (string, error) {
	addr := cfg.addr()
	lower := strings.ToLower(addr)
	if strings.Contains(lower, "/var/run/docker.sock") || strings.Contains(lower, "/run/docker.sock") || strings.HasPrefix(lower, "docker-container://") || strings.HasPrefix(lower, "npipe://") {
		return "", fmt.Errorf("buildkit addr must point to buildkitd, not a Docker endpoint: %s", addr)
	}
	return addr, nil
}

type buildkitSolver interface {
	Solve(context.Context, *llb.Definition, bkclient.SolveOpt, chan *bkclient.SolveStatus) (*bkclient.SolveResponse, error)
}

type Builder struct {
	client         buildkitSolver
	outputRoot     string
	cacheNamespace string
	health         interface {
		Check(context.Context) error
	}
}

type ServiceFailure struct {
	Cause error
}

func (e *ServiceFailure) Error() string {
	if e == nil || e.Cause == nil {
		return "build service failure"
	}
	return "build service failure: " + e.Cause.Error()
}

func (e *ServiceFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (*ServiceFailure) FatalWorker() bool {
	return true
}

func Open(ctx context.Context, cfg Config) (*Builder, func() error, error) {
	addr, err := cfg.endpoint()
	if err != nil {
		return nil, nil, err
	}
	client, err := bkclient.New(ctx, addr)
	if err != nil {
		return nil, nil, err
	}
	b := New(client, cfg.OutputRoot, cfg.CacheNamespace)
	return b, client.Close, nil
}

func New(client buildkitSolver, outputRoot string, cacheNamespace ...string) *Builder {
	b := &Builder{
		client:         client,
		outputRoot:     outputRoot,
		cacheNamespace: defaultCacheNS,
	}
	if len(cacheNamespace) > 0 && strings.TrimSpace(cacheNamespace[0]) != "" {
		b.cacheNamespace = safeNamespace(cacheNamespace[0])
	}
	if strings.TrimSpace(b.outputRoot) == "" {
		b.outputRoot = filepath.Join(os.TempDir(), defaultOutputRoot)
	}
	return b
}

func (b *Builder) Build(ctx context.Context, request builder.Request) (builder.Artifact, error) {
	if b.client == nil {
		return builder.Artifact{}, errors.New("buildkit client is required")
	}
	if request.Bundle == nil || request.Bundle.Image == nil {
		return builder.Artifact{}, errors.New("bundle image is required")
	}
	if strings.TrimSpace(request.Source.ProjectRoot) == "" {
		return builder.Artifact{}, errors.New("source project root is required")
	}
	plan, err := planImage(
		request.Bundle.Image,
		request.Bundle.SubImages,
		request.Source.ProjectRoot,
		defaultPlatform,
		b.cacheNamespaceFor(request.CacheScope, request.TaskID),
	)
	if err != nil {
		return builder.Artifact{}, err
	}
	if err := validateBuildSecrets(request.Bundle.Image, request.Bundle.SubImages, request.BuildSecrets); err != nil {
		return builder.Artifact{}, err
	}
	return b.solve(ctx, imageSolveRequest{
		runID:        request.RunID,
		itemID:       request.TaskID,
		sourceSHA:    request.Source.SHA,
		plan:         plan,
		buildSecrets: request.BuildSecrets,
	})
}

func (b *Builder) BuildImage(
	ctx context.Context,
	request builder.ImageRequest,
) (builder.Artifact, error) {
	if b.client == nil {
		return builder.Artifact{}, errors.New("buildkit client is required")
	}
	if strings.TrimSpace(request.Source.ProjectRoot) == "" {
		return builder.Artifact{}, errors.New("source project root is required")
	}
	plan, err := planDeclaredImage(
		request.Build,
		request.Source.ProjectRoot,
		b.cacheNamespaceFor(request.CacheScope, request.WorkspaceID),
	)
	if err != nil {
		return builder.Artifact{}, err
	}
	return b.solve(ctx, imageSolveRequest{
		runID:     request.RunID,
		itemID:    request.WorkspaceID,
		sourceSHA: request.Source.SHA,
		plan:      plan,
	})
}

type imageSolveRequest struct {
	runID        string
	itemID       string
	sourceSHA    string
	plan         llbPlan
	buildSecrets map[string][]byte
}

func (b *Builder) solve(
	ctx context.Context,
	request imageSolveRequest,
) (builder.Artifact, error) {
	output, err := b.output(request.runID, request.itemID)
	if err != nil {
		return builder.Artifact{}, err
	}
	platform, err := platformSpec(request.plan.Platform)
	if err != nil {
		return builder.Artifact{}, err
	}
	definition, err := request.plan.State.Marshal(ctx, llb.Platform(platform))
	if err != nil {
		return builder.Artifact{}, fmt.Errorf("marshal build graph: %w", err)
	}
	configJSON, err := json.Marshal(request.plan.Config)
	if err != nil {
		return builder.Artifact{}, fmt.Errorf("encode image config: %w", err)
	}
	imageFile, err := os.Create(output.imageTar)
	if err != nil {
		return builder.Artifact{}, fmt.Errorf("create image tar: %w", err)
	}
	closeImage := func() error {
		if imageFile == nil {
			return nil
		}
		err := imageFile.Close()
		imageFile = nil
		return err
	}
	defer func() { _ = closeImage() }()

	response, err := b.client.Solve(ctx, definition, bkclient.SolveOpt{
		LocalMounts: request.plan.LocalMounts,
		Exports: []bkclient.ExportEntry{{
			Type: bkclient.ExporterOCI,
			Attrs: map[string]string{
				"name":                          "helmr/" + safeSegment(request.runID),
				"platform-split":                "false",
				exptypes.ExporterImageConfigKey: string(configJSON),
			},
			Output: func(map[string]string) (io.WriteCloser, error) {
				return noCloseWriteCloser{Writer: imageFile}, nil
			},
		}},
		Session: buildSecretSession(request.buildSecrets),
	}, nil)
	if err != nil {
		closeErr := closeImage()
		removeErr := os.RemoveAll(output.root)
		solveErr := b.solveError(ctx, err, request.buildSecrets)
		if closeErr != nil || removeErr != nil {
			return builder.Artifact{}, &ServiceFailure{Cause: errors.Join(solveErr, closeErr, removeErr)}
		}
		return builder.Artifact{}, solveErr
	}
	if err := closeImage(); err != nil {
		removeErr := os.RemoveAll(output.root)
		return builder.Artifact{}, &ServiceFailure{Cause: errors.Join(fmt.Errorf("close image tar: %w", err), removeErr)}
	}
	if err := os.WriteFile(output.config, configJSON, 0o644); err != nil {
		return builder.Artifact{}, err
	}
	if err := writeJSONFile(output.manifest, map[string]any{
		"kind":      "buildkit-oci-tar",
		"runID":     request.runID,
		"itemID":    request.itemID,
		"sourceSHA": request.sourceSHA,
		"platform":  request.plan.Platform,
		"exporter":  exporterResponse(response),
	}); err != nil {
		return builder.Artifact{}, err
	}
	return builder.Artifact{RootPath: output.root, ImageTarPath: output.imageTar, ConfigPath: output.config, ManifestPath: output.manifest}, nil
}

func (b *Builder) solveError(ctx context.Context, solveErr error, secrets map[string][]byte) error {
	redacted := errors.New("solve build graph: " + redactBuildError(solveErr, secrets))
	if b.health == nil {
		return redacted
	}
	switch grpcerrors.Code(solveErr) {
	case codes.Unavailable, codes.Internal, codes.ResourceExhausted:
		return &ServiceFailure{Cause: redacted}
	}
	healthCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildKitHealthTimeout)
	defer cancel()
	if err := b.health.Check(healthCtx); err != nil {
		return &ServiceFailure{Cause: errors.Join(redacted, fmt.Errorf("prove BuildKit service generation healthy: %w", err))}
	}
	if ctx.Err() != nil {
		return errors.Join(redacted, context.Cause(ctx))
	}
	return redacted
}

func (b *Builder) output(runID, itemID string) (buildOutput, error) {
	root := filepath.Join(b.outputRoot, safeSegment(runID), safeSegment(itemID))
	if err := os.RemoveAll(root); err != nil {
		return buildOutput{}, fmt.Errorf("clean build output: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return buildOutput{}, fmt.Errorf("create build output: %w", err)
	}
	return buildOutput{
		root:     root,
		imageTar: filepath.Join(root, "image.oci.tar"),
		config:   filepath.Join(root, "config.json"),
		manifest: filepath.Join(root, "manifest.json"),
	}, nil
}

func (b *Builder) cacheNamespaceFor(scope, itemID string) string {
	scope = safeNamespace(scope)
	if scope == "_" {
		scope = safeSegment(itemID)
	}
	if scope == "_" {
		return b.cacheNamespace
	}
	return b.cacheNamespace + "/" + scope
}

type buildOutput struct {
	root     string
	imageTar string
	config   string
	manifest string
}

type noCloseWriteCloser struct {
	io.Writer
}

func (noCloseWriteCloser) Close() error { return nil }

func writeJSONFile(path string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func safeSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "_"
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func safeNamespace(value string) string {
	segments := strings.FieldsFunc(value, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	safe := make([]string, 0, len(segments))
	for _, segment := range segments {
		if next := safeSegment(segment); next != "_" {
			safe = append(safe, next)
		}
	}
	if len(safe) == 0 {
		return "_"
	}
	return strings.Join(safe, "/")
}

func buildSecretSession(secrets map[string][]byte) []session.Attachable {
	if len(secrets) == 0 {
		return nil
	}
	return []session.Attachable{secretsprovider.FromMap(secrets)}
}

func exporterResponse(response *bkclient.SolveResponse) map[string]string {
	if response == nil || len(response.ExporterResponse) == 0 {
		return nil
	}
	return response.ExporterResponse
}
