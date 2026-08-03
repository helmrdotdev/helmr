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
	"sync"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/session"
)

const (
	defaultOutputRoot = "helmr-worker-builds"
	defaultPlatform   = "linux/amd64"
)

type buildkitSolver interface {
	Solve(context.Context, *llb.Definition, bkclient.SolveOpt, chan *bkclient.SolveStatus) (*bkclient.SolveResponse, error)
}

type Builder struct {
	client         buildkitSolver
	outputRoot     string
	ociOutputLimit int64
	sessions       []session.Attachable
}

type OutputQuotaFailure struct {
	LimitBytes int64
}

func (failure *OutputQuotaFailure) Error() string {
	return fmt.Sprintf("image-build OCI output exceeds the %d-byte contract", failure.LimitBytes)
}

func New(client buildkitSolver, outputRoot string) *Builder {
	b := &Builder{
		client:         client,
		outputRoot:     outputRoot,
		ociOutputLimit: imagebuild.MaxOCIArchiveBytes,
	}
	if strings.TrimSpace(b.outputRoot) == "" {
		b.outputRoot = filepath.Join(os.TempDir(), defaultOutputRoot)
	}
	return b
}

func (b *Builder) BuildImage(
	ctx context.Context,
	request imagebuild.Request,
) (imagebuild.Artifact, error) {
	if b.client == nil {
		return imagebuild.Artifact{}, errors.New("BuildKit client is required")
	}
	if strings.TrimSpace(request.Source.ProjectRoot) == "" {
		return imagebuild.Artifact{}, errors.New("source project root is required")
	}
	plan, err := planDeclaredImage(
		request.Build,
		request.Source.ProjectRoot,
	)
	if err != nil {
		return imagebuild.Artifact{}, err
	}
	return b.solve(ctx, imageSolveRequest{
		runID:     request.RunID,
		itemID:    request.WorkspaceID,
		sourceSHA: request.Source.SHA,
		plan:      plan,
		cache:     request.Cache,
	})
}

type imageSolveRequest struct {
	runID     string
	itemID    string
	sourceSHA string
	plan      llbPlan
	cache     *imagebuild.CacheBinding
}

func (b *Builder) solve(
	ctx context.Context,
	request imageSolveRequest,
) (imagebuild.Artifact, error) {
	output, err := b.output(request.runID, request.itemID)
	if err != nil {
		return imagebuild.Artifact{}, err
	}
	platform, err := platformSpec(request.plan.Platform)
	if err != nil {
		return imagebuild.Artifact{}, err
	}
	definition, err := request.plan.State.Marshal(ctx, llb.Platform(platform))
	if err != nil {
		return imagebuild.Artifact{}, fmt.Errorf("marshal build graph: %w", err)
	}
	configJSON, err := json.Marshal(request.plan.Config)
	if err != nil {
		return imagebuild.Artifact{}, fmt.Errorf("encode image config: %w", err)
	}
	imageFile, err := os.Create(output.imageTar)
	if err != nil {
		return imagebuild.Artifact{}, fmt.Errorf("create image tar: %w", err)
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
	imageOutput := &boundedWriteCloser{
		writer: imageFile,
		limit:  b.ociOutputLimit,
	}

	solveOptions := bkclient.SolveOpt{
		LocalMounts: request.plan.LocalMounts,
		Session:     b.sessions,
		Exports: []bkclient.ExportEntry{{
			Type: bkclient.ExporterOCI,
			Attrs: map[string]string{
				"name":                          "helmr/" + safeSegment(request.runID),
				"platform-split":                "false",
				exptypes.ExporterImageConfigKey: string(configJSON),
			},
			Output: func(map[string]string) (io.WriteCloser, error) {
				return imageOutput, nil
			},
		}},
	}
	if request.cache != nil {
		solveOptions.CacheImports = []bkclient.CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref": request.cache.Ref,
			},
		}}
		solveOptions.CacheExports = []bkclient.CacheOptionsEntry{{
			Type: "registry",
			Attrs: map[string]string{
				"ref":            request.cache.Ref,
				"mode":           "max",
				"oci-mediatypes": "true",
				"image-manifest": "true",
				"ignore-error":   "true",
			},
		}}
	}
	response, err := b.client.Solve(ctx, definition, solveOptions, nil)
	if imageOutput.exceededQuota() {
		err = &OutputQuotaFailure{LimitBytes: b.ociOutputLimit}
	}
	if err != nil {
		closeErr := closeImage()
		removeErr := os.RemoveAll(output.root)
		return imagebuild.Artifact{}, errors.Join(b.solveError(ctx, err), closeErr, removeErr)
	}
	if err := closeImage(); err != nil {
		removeErr := os.RemoveAll(output.root)
		return imagebuild.Artifact{}, errors.Join(fmt.Errorf("close image tar: %w", err), removeErr)
	}
	if err := os.WriteFile(output.config, configJSON, 0o644); err != nil {
		return imagebuild.Artifact{}, err
	}
	if err := writeJSONFile(output.manifest, map[string]any{
		"kind":      "buildkit-oci-tar",
		"runID":     request.runID,
		"itemID":    request.itemID,
		"sourceSHA": request.sourceSHA,
		"platform":  request.plan.Platform,
		"exporter":  exporterResponse(response),
	}); err != nil {
		return imagebuild.Artifact{}, err
	}
	return imagebuild.Artifact{RootPath: output.root, ImageTarPath: output.imageTar, ConfigPath: output.config, ManifestPath: output.manifest}, nil
}

func (b *Builder) solveError(ctx context.Context, solveErr error) error {
	failure := fmt.Errorf("solve build graph: %w", solveErr)
	if ctx.Err() != nil {
		return errors.Join(failure, context.Cause(ctx))
	}
	return failure
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

type buildOutput struct {
	root     string
	imageTar string
	config   string
	manifest string
}

type boundedWriteCloser struct {
	mu       sync.Mutex
	writer   io.Writer
	limit    int64
	written  int64
	exceeded bool
}

func (writer *boundedWriteCloser) Write(content []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	remaining := writer.limit - writer.written
	if remaining >= int64(len(content)) {
		written, err := writer.writer.Write(content)
		writer.written += int64(written)
		return written, err
	}
	allowed := max(remaining, 0)
	written := 0
	if allowed > 0 {
		count, err := writer.writer.Write(content[:allowed])
		written = count
		writer.written += int64(count)
		if err != nil {
			return written, err
		}
		if int64(count) != allowed {
			return written, io.ErrShortWrite
		}
	}
	writer.exceeded = true
	return written, &OutputQuotaFailure{LimitBytes: writer.limit}
}

func (*boundedWriteCloser) Close() error { return nil }

func (writer *boundedWriteCloser) exceededQuota() bool {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.exceeded
}

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

func exporterResponse(response *bkclient.SolveResponse) map[string]string {
	if response == nil || len(response.ExporterResponse) == 0 {
		return nil
	}
	return response.ExporterResponse
}
