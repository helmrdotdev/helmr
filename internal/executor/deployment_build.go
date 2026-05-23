package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	bundlev0 "github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

type DeploymentBuilder struct {
	WorkDir  string
	CAS      cas.Store
	Indexer  DeploymentIndexer
	Compiler Compiler
}

type DeploymentIndexer interface {
	Index(context.Context, DeploymentIndexRequest) (DeploymentIndex, error)
}

type DeploymentIndexRequest struct {
	Source builder.Source
	RunID  string
}

type DeploymentIndex struct {
	Tasks map[string]DeploymentIndexTask
}

type DeploymentIndexTask struct {
	FilePath   string
	ExportName string
}

type GuestDeploymentIndexer struct {
	Connector vm.Connector
	TempDir   string
}

func (p GuestDeploymentIndexer) Index(ctx context.Context, request DeploymentIndexRequest) (DeploymentIndex, error) {
	if p.Connector == nil {
		return DeploymentIndex{}, errors.New("deployment indexer guest connector is required")
	}
	source := request.Source
	if strings.TrimSpace(source.ProjectRoot) == "" {
		return DeploymentIndex{}, errors.New("source project root is required")
	}
	sourceTar, cleanup, err := sourcetar.CreateTar(source.ProjectRoot, p.TempDir)
	if err != nil {
		return DeploymentIndex{}, err
	}
	defer cleanup()

	session, err := p.Connector.Connect(ctx)
	if err != nil {
		return DeploymentIndex{}, fmt.Errorf("connect deployment indexer guest: %w", err)
	}
	defer session.Close()
	stream := session.Stream()

	runID := strings.TrimSpace(request.RunID)
	if runID == "" {
		runID = "deployment-index"
	}
	if err := writeFileFrame(stream, transport.StreamHeader{Type: transport.StreamTypeIndexSource, RunID: runID}, sourceTar.Path); err != nil {
		return DeploymentIndex{}, fmt.Errorf("write deployment source: %w", err)
	}
	body, err := transport.ReadMessageFrame(stream)
	if err != nil {
		return DeploymentIndex{}, fmt.Errorf("read deployment index: %w", err)
	}
	if frame, ok, err := transport.DecodeParseErrorFrame(body); err != nil {
		return DeploymentIndex{}, fmt.Errorf("read deployment index: %w", err)
	} else if ok {
		return DeploymentIndex{}, TaskParseError{Kind: frame.Kind, Message: frame.Message}
	}
	return decodeDeploymentIndex(body)
}

func (e DeploymentBuilder) BuildDeployment(ctx context.Context, lease api.WorkerDeploymentBuildLease, deployment api.WorkerDeploymentBuild) api.WorkerDeploymentBuildResult {
	if e.CAS == nil {
		return failedDeploymentBuild(errors.New("deployment build CAS is required"))
	}
	if e.Indexer == nil {
		return failedDeploymentBuild(errors.New("deployment indexer is required"))
	}
	if e.Compiler == nil {
		return failedDeploymentBuild(ErrCompilerRequired)
	}
	source, cleanup, err := (Executor{WorkDir: e.WorkDir, CAS: e.CAS}).materializeSourceArtifact(ctx, deployment.SourceArtifact, "deployment")
	if err != nil {
		return failedDeploymentBuild(err)
	}
	defer cleanup()

	index, err := e.Indexer.Index(ctx, DeploymentIndexRequest{Source: source, RunID: lease.DeploymentID})
	if err != nil {
		return failedDeploymentBuild(err)
	}
	taskIDs := make([]string, 0, len(index.Tasks))
	for taskID := range index.Tasks {
		taskIDs = append(taskIDs, taskID)
	}
	sort.Strings(taskIDs)
	if len(taskIDs) == 0 {
		return failedDeploymentBuild(errors.New("deployment source must contain at least one task"))
	}

	tasks := make([]api.WorkerDeploymentBuildTask, 0, len(taskIDs))
	casObjects := make([]api.CASObject, 0, len(taskIDs)+2)
	for _, taskID := range taskIDs {
		indexTask := index.Tasks[taskID]
		if err := api.ValidateTaskID(taskID); err != nil {
			return failedDeploymentBuild(err)
		}
		bundle, err := e.Compiler.Compile(ctx, CompileRequest{Source: source, TaskID: taskID})
		if err != nil {
			return failedDeploymentBuild(err)
		}
		filePath := strings.TrimSpace(indexTask.FilePath)
		if filePath == "" && bundle != nil && bundle.Task != nil {
			filePath = strings.TrimSpace(bundle.Task.ModulePath)
		}
		if filePath == "" {
			return failedDeploymentBuild(fmt.Errorf("task %q file_path is required", taskID))
		}
		exportName := strings.TrimSpace(indexTask.ExportName)
		if exportName == "" {
			return failedDeploymentBuild(fmt.Errorf("task %q export_name is required", taskID))
		}
		resources, err := deploymentTaskResources(bundle)
		if err != nil {
			return failedDeploymentBuild(fmt.Errorf("task %q resources: %w", taskID, err))
		}
		body, err := proto.Marshal(bundle)
		if err != nil {
			return failedDeploymentBuild(fmt.Errorf("marshal task %q bundle: %w", taskID, err))
		}
		object, err := e.CAS.Put(ctx, api.TaskBundleArtifactMediaType, bytes.NewReader(body))
		if err != nil {
			return failedDeploymentBuild(fmt.Errorf("store task %q bundle: %w", taskID, err))
		}
		casObjects = append(casObjects, api.CASObject{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType})
		tasks = append(tasks, api.WorkerDeploymentBuildTask{
			TaskID:             taskID,
			FilePath:           filePath,
			ExportName:         exportName,
			HandlerEntrypoint:  filePath + "#" + exportName,
			BundleDigest:       object.Digest,
			RequestedMilliCPU:  resources.MilliCPU,
			RequestedMemoryMiB: resources.MemoryMiB,
			MaxDurationSeconds: 300,
		})
	}

	manifest := map[string]any{
		"deployment_id": deployment.ID,
		"source_digest": deployment.SourceArtifact.Digest,
		"tasks":         tasks,
	}
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		return failedDeploymentBuild(fmt.Errorf("marshal deployment manifest: %w", err))
	}
	deploymentManifest, err := e.CAS.Put(ctx, api.DeploymentManifestArtifactMediaType, bytes.NewReader(manifestBody))
	if err != nil {
		return failedDeploymentBuild(fmt.Errorf("store deployment manifest: %w", err))
	}
	buildManifestBody, err := json.Marshal(map[string]any{
		"deployment_id":              deployment.ID,
		"source_digest":              deployment.SourceArtifact.Digest,
		"deployment_manifest_digest": deploymentManifest.Digest,
	})
	if err != nil {
		return failedDeploymentBuild(fmt.Errorf("marshal build manifest: %w", err))
	}
	buildManifest, err := e.CAS.Put(ctx, api.BuildManifestArtifactMediaType, bytes.NewReader(buildManifestBody))
	if err != nil {
		return failedDeploymentBuild(fmt.Errorf("store build manifest: %w", err))
	}
	casObjects = append(casObjects,
		api.CASObject{Digest: deploymentManifest.Digest, SizeBytes: deploymentManifest.SizeBytes, MediaType: deploymentManifest.MediaType},
		api.CASObject{Digest: buildManifest.Digest, SizeBytes: buildManifest.SizeBytes, MediaType: buildManifest.MediaType},
	)
	return api.WorkerDeploymentBuildResult{
		BuildManifestDigest:      buildManifest.Digest,
		DeploymentManifestDigest: deploymentManifest.Digest,
		ContentHash:              deploymentManifest.Digest,
		Tasks:                    tasks,
		CASObjects:               casObjects,
	}
}

func failedDeploymentBuild(err error) api.WorkerDeploymentBuildResult {
	message := err.Error()
	return api.WorkerDeploymentBuildResult{Error: &message}
}

func decodeDeploymentIndex(body []byte) (DeploymentIndex, error) {
	var payload struct {
		Tasks map[string]struct {
			OriginFile string `json:"originFile"`
			ModulePath string `json:"modulePath"`
			ExportName string `json:"exportName"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return DeploymentIndex{}, fmt.Errorf("decode deployment index: %w", err)
	}
	index := DeploymentIndex{Tasks: make(map[string]DeploymentIndexTask, len(payload.Tasks))}
	for taskID, task := range payload.Tasks {
		filePath := strings.TrimSpace(task.ModulePath)
		if filePath == "" {
			filePath = strings.TrimSpace(task.OriginFile)
		}
		index.Tasks[taskID] = DeploymentIndexTask{
			FilePath:   filePath,
			ExportName: strings.TrimSpace(task.ExportName),
		}
	}
	return index, nil
}

func deploymentTaskResources(bundle *bundlev0.Bundle) (compute.ResourceVector, error) {
	resources := compute.DefaultRunResources()
	if bundle == nil || bundle.GetSandbox() == nil || bundle.GetSandbox().GetResources() == nil {
		return resources, resources.Validate(true)
	}
	input := bundle.GetSandbox().GetResources()
	if input.GetCpu() != 0 {
		resources.MilliCPU = int64(input.GetCpu()) * 1000
	}
	if memory := strings.TrimSpace(input.GetMemory()); memory != "" {
		memoryMiB, err := parseMemoryMiB(memory)
		if err != nil {
			return compute.ResourceVector{}, err
		}
		resources.MemoryMiB = memoryMiB
	}
	return resources, resources.Validate(true)
}

func parseMemoryMiB(input string) (int64, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return 0, errors.New("memory is required")
	}
	units := []struct {
		suffix     string
		multiplier int64
	}{
		{suffix: "kib", multiplier: 1},
		{suffix: "ki", multiplier: 1},
		{suffix: "mib", multiplier: 1024},
		{suffix: "mi", multiplier: 1024},
		{suffix: "gib", multiplier: 1024 * 1024},
		{suffix: "gi", multiplier: 1024 * 1024},
	}
	lower := strings.ToLower(value)
	for _, unit := range units {
		if strings.HasSuffix(lower, unit.suffix) {
			amountText := strings.TrimSpace(value[:len(value)-len(unit.suffix)])
			amount, err := strconv.ParseInt(amountText, 10, 64)
			if err != nil || amount <= 0 {
				return 0, fmt.Errorf("memory %q must be a positive integer quantity", input)
			}
			if unit.multiplier == 1 {
				if amount%1024 != 0 {
					return 0, fmt.Errorf("memory %q must resolve to whole MiB", input)
				}
				return amount / 1024, nil
			}
			return amount * unit.multiplier / 1024, nil
		}
	}
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || amount <= 0 {
		return 0, fmt.Errorf("memory %q must use MiB or GiB units", input)
	}
	return amount, nil
}
