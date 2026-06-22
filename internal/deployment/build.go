package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/stablejson"
	"github.com/helmrdotdev/helmr/internal/task"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

type Builder struct {
	WorkDir      string
	CAS          cas.Store
	Indexer      Indexer
	Compiler     task.Compiler
	ImageBuilder builder.Engine
}

type Indexer interface {
	Index(context.Context, IndexRequest) (Catalog, error)
}

type IndexRequest struct {
	Source builder.Source
	RunID  string
}

type Catalog struct {
	Tasks map[string]CatalogTask
}

type CatalogTask struct {
	FilePath   string
	ExportName string
}

type GuestIndexer struct {
	Connector vm.Connector
	TempDir   string
}

func (p GuestIndexer) Index(ctx context.Context, request IndexRequest) (Catalog, error) {
	if p.Connector == nil {
		return Catalog{}, errors.New("deployment indexer guest connector is required")
	}
	source := request.Source
	if strings.TrimSpace(source.ProjectRoot) == "" {
		return Catalog{}, errors.New("source project root is required")
	}
	sourceTar, cleanup, err := archive.CreateTarWithOptions(source.ProjectRoot, p.TempDir, archive.TarOptions{
		ExcludePatterns: []string{"**/.git/**"},
	})
	if err != nil {
		return Catalog{}, err
	}
	defer cleanup()

	session, err := p.Connector.Connect(ctx, compute.DefaultNetworkPolicy())
	if err != nil {
		return Catalog{}, fmt.Errorf("connect deployment indexer guest: %w", err)
	}
	defer session.Close(context.Background())
	stream := session.Stream()

	runID := strings.TrimSpace(request.RunID)
	if runID == "" {
		runID = "deployment-index"
	}
	if err := transport.WriteFileFrame(stream, transport.StreamHeader{Type: transport.StreamTypeCatalogDeployment, RunID: runID}, sourceTar.Path); err != nil {
		return Catalog{}, fmt.Errorf("write deployment source: %w", err)
	}
	body, err := transport.ReadMessageFrame(stream)
	if err != nil {
		return Catalog{}, fmt.Errorf("read deployment index: %w", err)
	}
	if frame, ok, err := transport.DecodeParseErrorFrame(body); err != nil {
		return Catalog{}, fmt.Errorf("read deployment index: %w", err)
	} else if ok {
		return Catalog{}, task.ParseError{Kind: frame.Kind, Message: frame.Message}
	}
	return decodeCatalog(body)
}

func (e Builder) BuildDeployment(ctx context.Context, lease api.WorkerDeploymentBuildLease, deployment api.WorkerDeploymentBuild) api.WorkerDeploymentBuildResult {
	if e.CAS == nil {
		return failedDeploymentBuild(errors.New("deployment build CAS is required"))
	}
	if e.Indexer == nil {
		return failedDeploymentBuild(errors.New("deployment indexer is required"))
	}
	if e.Compiler == nil {
		return failedDeploymentBuild(task.ErrCompilerRequired)
	}
	if e.ImageBuilder == nil {
		return failedDeploymentBuild(errors.New("deployment image builder is required"))
	}
	source, cleanup, err := materializeSourceArtifact(ctx, e.WorkDir, e.CAS, deployment.DeploymentSource, "deployment")
	if err != nil {
		return failedDeploymentBuild(err)
	}
	defer cleanup()

	index, err := e.Indexer.Index(ctx, IndexRequest{Source: source, RunID: lease.DeploymentID})
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
	casObjects := make([]api.CASObject, 0, len(taskIDs)*2+2)
	sandboxImages := map[string]api.CASObject{}
	for _, taskID := range taskIDs {
		indexTask := index.Tasks[taskID]
		if err := api.ValidateTaskID(taskID); err != nil {
			return failedDeploymentBuild(err)
		}
		bundle, err := e.Compiler.Compile(ctx, task.CompileRequest{Source: source, TaskID: taskID})
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
		network, err := deploymentTaskNetwork(bundle)
		if err != nil {
			return failedDeploymentBuild(fmt.Errorf("task %q network: %w", taskID, err))
		}
		sandbox, err := deploymentTaskSandbox(bundle)
		if err != nil {
			return failedDeploymentBuild(fmt.Errorf("task %q sandbox: %w", taskID, err))
		}
		maxDurationSeconds, err := deploymentTaskMaxDurationSeconds(bundle)
		if err != nil {
			return failedDeploymentBuild(fmt.Errorf("task %q max duration: %w", taskID, err))
		}
		schedules := deploymentTaskSchedules(bundle)
		imageObject, ok := sandboxImages[sandbox.id]
		if !ok {
			imageArtifact, err := e.ImageBuilder.Build(ctx, builder.Request{
				RunID:  lease.DeploymentID,
				TaskID: taskID,
				Bundle: bundle,
				Source: source,
			})
			if err != nil {
				return failedDeploymentBuild(fmt.Errorf("build sandbox %q image: %w", sandbox.id, err))
			}
			imageFile, err := os.Open(imageArtifact.ImageTarPath)
			if err != nil {
				cleanupBuildArtifact(imageArtifact)
				return failedDeploymentBuild(fmt.Errorf("open sandbox %q image artifact: %w", sandbox.id, err))
			}
			object, putErr := e.CAS.Put(ctx, api.SandboxImageArtifactMediaType, imageFile)
			closeErr := imageFile.Close()
			cleanupBuildArtifact(imageArtifact)
			if putErr != nil {
				return failedDeploymentBuild(fmt.Errorf("store sandbox %q image artifact: %w", sandbox.id, putErr))
			}
			if closeErr != nil {
				return failedDeploymentBuild(fmt.Errorf("close sandbox %q image artifact: %w", sandbox.id, closeErr))
			}
			imageObject = api.CASObject{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
			sandboxImages[sandbox.id] = imageObject
			casObjects = append(casObjects, imageObject)
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
		concurrencyLimit, err := deploymentTaskConcurrencyLimit(bundle)
		if err != nil {
			return failedDeploymentBuild(fmt.Errorf("task %q concurrency limit: %w", taskID, err))
		}
		tasks = append(tasks, api.WorkerDeploymentBuildTask{
			TaskID:                     taskID,
			SandboxID:                  sandbox.id,
			SandboxFingerprint:         sandbox.fingerprint,
			SandboxImageArtifact:       imageObject,
			SandboxImageArtifactFormat: "oci-tar",
			SandboxImageDigest:         imageObject.Digest,
			SandboxImageFormat:         "oci-tar",
			WorkspaceMountPath:         sandbox.workspaceMountPath,
			FilesystemFormat:           sandbox.filesystemFormat,
			FilePath:                   filePath,
			ExportName:                 exportName,
			HandlerEntrypoint:          filePath + "#" + exportName,
			BundleDigest:               object.Digest,
			BundleFormatVersion:        api.CurrentBundleFormatVersion,
			RequestedMilliCPU:          resources.MilliCPU,
			RequestedMemoryMiB:         resources.MemoryMiB,
			RequestedDiskMiB:           resources.DiskMiB,
			Network:                    network,
			QueueName:                  deploymentTaskQueueName(bundle, taskID),
			ConcurrencyLimit:           concurrencyLimit,
			TTL:                        deploymentTaskTTL(bundle),
			MaxDurationSeconds:         maxDurationSeconds,
			RetryPolicy:                deploymentTaskRetryPolicy(bundle),
			Secrets:                    deploymentTaskSecrets(bundle),
			Schedules:                  schedules,
		})
	}

	manifest := map[string]any{
		"deployment_id":            deployment.ID,
		"deployment_version":       deployment.Version,
		"api_version":              deployment.APIVersion,
		"sdk_version":              deployment.SDKVersion,
		"cli_version":              deployment.CLIVersion,
		"bundle_format_version":    deployment.BundleFormatVersion,
		"worker_protocol_version":  deployment.WorkerProtocolVersion,
		"deployment_source_digest": deployment.DeploymentSource.Digest,
		"tasks":                    tasks,
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
		"deployment_version":         deployment.Version,
		"api_version":                deployment.APIVersion,
		"sdk_version":                deployment.SDKVersion,
		"cli_version":                deployment.CLIVersion,
		"bundle_format_version":      deployment.BundleFormatVersion,
		"worker_protocol_version":    deployment.WorkerProtocolVersion,
		"deployment_source_digest":   deployment.DeploymentSource.Digest,
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
		Tasks:                    tasks,
		CASObjects:               casObjects,
	}
}

func failedDeploymentBuild(err error) api.WorkerDeploymentBuildResult {
	message := err.Error()
	return api.WorkerDeploymentBuildResult{Error: &message}
}

type deploymentSandboxBuildMetadata struct {
	id                 string
	fingerprint        string
	workspaceMountPath string
	filesystemFormat   string
}

func deploymentTaskSandbox(bundle *bundlev0.Bundle) (deploymentSandboxBuildMetadata, error) {
	if bundle == nil || bundle.GetSandbox() == nil {
		return deploymentSandboxBuildMetadata{}, errors.New("sandbox is required")
	}
	sandbox := bundle.GetSandbox()
	sandboxID := strings.TrimSpace(sandbox.GetId())
	if sandboxID == "" {
		return deploymentSandboxBuildMetadata{}, errors.New("id is required")
	}
	workspace := sandbox.GetWorkspace()
	if workspace == nil || strings.TrimSpace(workspace.GetMountPath()) == "" {
		return deploymentSandboxBuildMetadata{}, errors.New("workspace mount_path is required")
	}
	fingerprint, err := sandboxContractFingerprint(bundle)
	if err != nil {
		return deploymentSandboxBuildMetadata{}, fmt.Errorf("sandbox fingerprint: %w", err)
	}
	return deploymentSandboxBuildMetadata{
		id:                 sandboxID,
		fingerprint:        fingerprint,
		workspaceMountPath: strings.TrimSpace(workspace.GetMountPath()),
		filesystemFormat:   "tar",
	}, nil
}

type sandboxContractFingerprintDocument struct {
	ContractVersion  int                             `json:"contract_version"`
	FilesystemFormat string                          `json:"filesystem_format"`
	Network          sandboxContractNetwork          `json:"network"`
	SandboxID        string                          `json:"sandbox_id"`
	Workspace        sandboxContractWorkspaceBinding `json:"workspace"`
}

type sandboxContractWorkspaceBinding struct {
	MountPath string `json:"mount_path"`
}

type sandboxContractNetwork struct {
	Allow    []string `json:"allow"`
	Deny     []string `json:"deny"`
	Internet bool     `json:"internet"`
}

func sandboxContractFingerprint(bundle *bundlev0.Bundle) (string, error) {
	if bundle == nil || bundle.GetSandbox() == nil {
		return "", errors.New("sandbox is required")
	}
	sandbox := bundle.GetSandbox()
	if sandbox.GetWorkspace() == nil {
		return "", errors.New("workspace is required")
	}
	network := compute.DefaultNetworkPolicy()
	if input := sandbox.GetNetwork(); input != nil {
		network = compute.NetworkPolicy{
			Internet: input.GetInternet(),
			Allow:    append([]string(nil), input.GetAllow()...),
			Deny:     append([]string(nil), input.GetDeny()...),
		}
	}
	sort.Strings(network.Allow)
	sort.Strings(network.Deny)
	document := sandboxContractFingerprintDocument{
		ContractVersion:  1,
		FilesystemFormat: "tar",
		Network: sandboxContractNetwork{
			Allow:    network.Allow,
			Deny:     network.Deny,
			Internet: network.Internet,
		},
		SandboxID: strings.TrimSpace(sandbox.GetId()),
		Workspace: sandboxContractWorkspaceBinding{
			MountPath: strings.TrimSpace(sandbox.GetWorkspace().GetMountPath()),
		},
	}
	body, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	canonical, err := stablejson.Encode(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func materializeSourceArtifact(ctx context.Context, workDir string, store cas.Store, artifact api.DeploymentSourceArtifact, label string) (builder.Source, func(), error) {
	if store == nil {
		return builder.Source{}, func() {}, errors.New("deployment source artifact CAS is required")
	}
	if strings.TrimSpace(workDir) == "" {
		workDir = filepath.Join(os.TempDir(), "helmr-worker")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return builder.Source{}, func() {}, fmt.Errorf("create worker work dir: %w", err)
	}
	destination, err := os.MkdirTemp(workDir, label+"-artifact-")
	if err != nil {
		return builder.Source{}, func() {}, fmt.Errorf("create deployment source artifact dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(destination) }
	body, err := store.Get(ctx, strings.TrimSpace(artifact.Digest))
	if err != nil {
		cleanup()
		return builder.Source{}, func() {}, fmt.Errorf("get deployment source artifact: %w", err)
	}
	if err := archive.ExtractTar(body, destination); err != nil {
		_ = body.Close()
		cleanup()
		return builder.Source{}, func() {}, fmt.Errorf("extract deployment source artifact: %w", err)
	}
	if err := body.Close(); err != nil {
		cleanup()
		return builder.Source{}, func() {}, fmt.Errorf("close deployment source artifact: %w", err)
	}
	return builder.Source{CheckoutRoot: destination, ProjectRoot: destination, SHA: strings.TrimSpace(artifact.Digest)}, cleanup, nil
}

func cleanupBuildArtifact(artifact builder.Artifact) {
	root := filepath.Clean(strings.TrimSpace(artifact.RootPath))
	if root == "" || root == "." || root == string(filepath.Separator) {
		return
	}
	_ = os.RemoveAll(root)
}

func decodeCatalog(body []byte) (Catalog, error) {
	var payload struct {
		Tasks map[string]struct {
			OriginFile string `json:"originFile"`
			ModulePath string `json:"modulePath"`
			ExportName string `json:"exportName"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Catalog{}, fmt.Errorf("decode deployment index: %w", err)
	}
	index := Catalog{Tasks: make(map[string]CatalogTask, len(payload.Tasks))}
	for taskID, task := range payload.Tasks {
		filePath := strings.TrimSpace(task.ModulePath)
		if filePath == "" {
			filePath = strings.TrimSpace(task.OriginFile)
		}
		index.Tasks[taskID] = CatalogTask{
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
	if disk := strings.TrimSpace(input.GetDisk()); disk != "" {
		diskMiB, err := parseDiskMiB(disk)
		if err != nil {
			return compute.ResourceVector{}, err
		}
		resources.DiskMiB = diskMiB
	}
	return resources, resources.Validate(true)
}

func deploymentTaskNetwork(bundle *bundlev0.Bundle) (compute.NetworkPolicy, error) {
	network := compute.DefaultNetworkPolicy()
	if bundle == nil || bundle.GetSandbox() == nil || bundle.GetSandbox().GetNetwork() == nil {
		return network, network.Validate()
	}
	input := bundle.GetSandbox().GetNetwork()
	network = compute.NetworkPolicy{
		Internet: input.GetInternet(),
		Allow:    append([]string(nil), input.GetAllow()...),
		Deny:     append([]string(nil), input.GetDeny()...),
	}
	if len(network.Allow) > 0 {
		return compute.NetworkPolicy{}, errors.New("network allow rules are not supported yet")
	}
	return network, network.Validate()
}

func deploymentTaskMaxDurationSeconds(bundle *bundlev0.Bundle) (int32, error) {
	if bundle == nil || bundle.GetTask() == nil {
		return 0, errors.New("bundle task is required")
	}
	value := bundle.GetTask().GetMaxDurationSeconds()
	if value == 0 {
		return 0, errors.New("bundle task max_duration_seconds is required")
	}
	if value > uint32(1<<31-1) {
		return 0, fmt.Errorf("max_duration_seconds %d exceeds int32", value)
	}
	return int32(value), nil
}

func deploymentTaskQueueName(bundle *bundlev0.Bundle, taskID string) string {
	if bundle == nil || bundle.GetTask() == nil || bundle.GetTask().GetQueue() == nil {
		return "task/" + taskID
	}
	queueName := strings.TrimSpace(bundle.GetTask().GetQueue().GetName())
	if queueName == "" {
		return "task/" + taskID
	}
	return queueName
}

func deploymentTaskConcurrencyLimit(bundle *bundlev0.Bundle) (*int32, error) {
	if bundle == nil || bundle.GetTask() == nil || bundle.GetTask().GetQueue() == nil {
		return nil, nil
	}
	value := bundle.GetTask().GetQueue().ConcurrencyLimit
	if value == nil {
		return nil, nil
	}
	if *value > uint32(1<<31-1) {
		return nil, fmt.Errorf("concurrency_limit %d exceeds int32", *value)
	}
	converted := int32(*value)
	return &converted, nil
}

func deploymentTaskTTL(bundle *bundlev0.Bundle) string {
	if bundle == nil || bundle.GetTask() == nil {
		return ""
	}
	return strings.TrimSpace(bundle.GetTask().GetTtl())
}

func deploymentTaskRetryPolicy(bundle *bundlev0.Bundle) json.RawMessage {
	if bundle == nil || bundle.GetTask() == nil {
		return nil
	}
	retryPolicy := strings.TrimSpace(bundle.GetTask().GetRetryPolicyJson())
	if retryPolicy == "" {
		return nil
	}
	return json.RawMessage(retryPolicy)
}

func deploymentTaskSchedules(bundle *bundlev0.Bundle) []api.WorkerDeploymentTaskSchedule {
	if bundle == nil || bundle.GetTask() == nil {
		return nil
	}
	specs := bundle.GetTask().GetSchedules()
	schedules := make([]api.WorkerDeploymentTaskSchedule, 0, len(specs))
	for _, spec := range specs {
		if spec == nil {
			continue
		}
		schedules = append(schedules, api.WorkerDeploymentTaskSchedule{
			ID:       strings.TrimSpace(spec.GetId()),
			Cron:     strings.TrimSpace(spec.GetCron()),
			Timezone: strings.TrimSpace(spec.GetTimezone()),
			Active:   spec.Active,
		})
	}
	return schedules
}

func parseMemoryMiB(input string) (int64, error) {
	return parseResourceMiB(input, "memory", math.MaxInt32)
}

func deploymentTaskSecrets(bundle *bundlev0.Bundle) []api.SecretDeclaration {
	if bundle == nil || bundle.GetTask() == nil {
		return nil
	}
	placements := bundle.GetTask().GetSecrets()
	secrets := make([]api.SecretDeclaration, 0, len(placements))
	for _, placement := range placements {
		if placement == nil {
			continue
		}
		item := api.SecretDeclaration{Name: strings.TrimSpace(placement.GetName())}
		runtimePlacement := placement.GetPlacement()
		if runtimePlacement == nil {
			secrets = append(secrets, item)
			continue
		}
		switch value := runtimePlacement.GetKind().(type) {
		case *bundlev0.Placement_Env:
			item.Env = strings.TrimSpace(value.Env.GetName())
		case *bundlev0.Placement_File:
			item.File = strings.TrimSpace(value.File.GetPath())
			item.Mode = strings.TrimSpace(value.File.GetMode())
			item.Owner = strings.TrimSpace(value.File.GetOwner())
		case *bundlev0.Placement_Dir:
			item.Dir = strings.TrimSpace(value.Dir.GetPath())
			item.Mode = strings.TrimSpace(value.Dir.GetMode())
			item.Owner = strings.TrimSpace(value.Dir.GetOwner())
		}
		secrets = append(secrets, item)
	}
	return secrets
}

func parseDiskMiB(input string) (int64, error) {
	return parseResourceMiB(input, "disk", math.MaxInt32)
}

func parseResourceMiB(input string, name string, maxMiB int64) (int64, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return 0, fmt.Errorf("%s is required", name)
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
				return 0, fmt.Errorf("%s %q must be a positive integer quantity", name, input)
			}
			if unit.multiplier == 1 {
				if amount%1024 != 0 {
					return 0, fmt.Errorf("%s %q must resolve to whole MiB", name, input)
				}
				amount /= 1024
				if amount > maxMiB {
					return 0, fmt.Errorf("%s %q exceeds max %d MiB", name, input, maxMiB)
				}
				return amount, nil
			}
			if amount > maxMiB/(unit.multiplier/1024) {
				return 0, fmt.Errorf("%s %q exceeds max %d MiB", name, input, maxMiB)
			}
			return amount * unit.multiplier / 1024, nil
		}
	}
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || amount <= 0 {
		return 0, fmt.Errorf("%s %q must use MiB or GiB units", name, input)
	}
	if amount > maxMiB {
		return 0, fmt.Errorf("%s %q exceeds max %d MiB", name, input, maxMiB)
	}
	return amount, nil
}
