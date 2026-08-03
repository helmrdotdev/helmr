package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/firecracker"
	imageworker "github.com/helmrdotdev/helmr/internal/imagebuild/worker"
	imagecacheecr "github.com/helmrdotdev/helmr/internal/imagecache/ecr"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/helmrdotdev/helmr/internal/vm"
	workerdaemon "github.com/helmrdotdev/helmr/internal/worker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	checkpointEncryptor, err := checkpoint.New(cfg.CheckpointKey)
	if err != nil {
		return fmt.Errorf("configure checkpoint encryption: %w", err)
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = executor.DefaultWorkDir()
	}
	networkConfig := firecracker.Config{
		JailerUID:               cfg.JailerUID,
		JailerGID:               cfg.JailerGID,
		StateDir:                filepath.Join(workDir, "vms", "guest"),
		NetworkLinkPool:         cfg.NetworkLinkPool,
		NetworkTranslationPool:  cfg.NetworkTranslationPool,
		NetworkResolverIPv4:     cfg.NetworkResolverIPv4,
		NetworkBlockedIPv4CIDRs: cfg.NetworkBlockedIPv4CIDRs,
		NetworkCapacity:         int(cfg.WorkerExecutionSlots + cfg.WorkerBuildExecutors),
		IPPath:                  cfg.IPPath,
		NFTPath:                 cfg.NFTPath,
	}
	networkReclaimer, err := firecracker.NewNetworkReclaimer(networkConfig)
	if err != nil {
		return fmt.Errorf("configure routed network reclaimer: %w", err)
	}
	supportsRun := slices.Contains(cfg.WorkerRoles, "run")
	supportsBuild := slices.Contains(cfg.WorkerRoles, "build")
	var buildPolicy *deployment.BuildPolicy
	var platformStore cas.ImmutableStore
	var squashfsEncoder string
	verifierCgroupRoot, err := workerdaemon.PrepareVerifierHost()
	if err != nil {
		return fmt.Errorf("prepare verifier host: %w", err)
	}
	serviceID := uuid.Must(uuid.NewV7()).String()
	process, err := workerdaemon.Acquire(workDir, workerdaemon.ProcessIdentity{ServiceID: serviceID, Roles: cfg.WorkerRoles})
	if err != nil {
		return fmt.Errorf("acquire worker supervisor singleton: %w", err)
	}
	defer process.Close()
	if err := os.Remove(filepath.Join(workDir, drainCompleteMarkerName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear stale drain marker: %w", err)
	}
	var startupRecovery *workerdaemon.RecoveryEvidence
	substrateCacheDir := filepath.Join(workDir, "substrate-cache")
	artifactCacheDir := filepath.Join(workDir, "artifact-cache")
	var buildStorageConfig *workerdaemon.BuildStorageConfig
	var buildScratchLeaseBytes uint64
	if supportsBuild {
		const mib = uint64(1024 * 1024)
		scratchFloorMiB := admissionDiskFloorMiB(true, cfg.VMScratchDiskMiB, cfg.WorkerDiskReserveMiB)
		storage := workerdaemon.BuildStorageConfig{
			CacheRoot:                     cfg.BuildCacheDir,
			ScratchRoot:                   cfg.BuildScratchDir,
			WorkDir:                       workDir,
			JailerRoot:                    cfg.JailerChrootDir,
			RequiredCacheBytes:            uint64(cfg.SubstrateCacheMaxMiB+cfg.ArtifactCacheMaxMiB) * mib,
			RequiredScratchBytes:          uint64(scratchFloorMiB+firecracker.BootCorpusMaxMiB) * mib,
			RequiredScratchAvailableBytes: 1,
		}
		if _, err := workerdaemon.ProveBuildStorage(storage); err != nil {
			return fmt.Errorf("prove build storage: %w", err)
		}
		buildStorageConfig = &storage
	}
	var controlPlaneClient *workerclient.Client
	workerCredential, err := resolveAuthenticatedWorkerCredential(ctx, cfg, workDir, func(credential workerCredentialFile) error {
		candidate, candidateErr := workerclient.New(cfg.ControlPlaneURL,
			workerclient.WithAuth(credential.WorkerInstanceID, credential.WorkerInstanceSecret),
			workerclient.WithService(serviceID, workerapi.CurrentProtocolVersion, supportsRun, supportsBuild),
		)
		if candidateErr != nil {
			return candidateErr
		}
		if candidateErr = candidate.AuthenticateWorker(ctx); candidateErr != nil {
			return candidateErr
		}
		controlPlaneClient = candidate
		return nil
	})
	if err != nil {
		return fmt.Errorf("configure authenticated control client: %w", err)
	}
	if supportsBuild {
		storage := *buildStorageConfig
		evidence, err := workerdaemon.RecoverLocalVMState(ctx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim)
		if err != nil {
			return fmt.Errorf("recover local worker state before build activation: %w", err)
		}
		if len(evidence.Quarantined) != 0 || len(evidence.QuarantineErrors) != 0 {
			return errors.New("build worker runtime state is not clean enough to activate its boot corpus")
		}
		startupRecovery = &evidence
		if err := firecracker.CleanRuntimes(workDir, ""); err != nil {
			return fmt.Errorf("clean stale firecracker runtimes: %w", err)
		}
		storage.RequiredScratchAvailableBytes = uint64(firecracker.BootCorpusMaxMiB) * 1024 * 1024
		storageProof, err := workerdaemon.ProveBuildStorage(storage)
		if err != nil {
			return fmt.Errorf("prove build storage after recovery: %w", err)
		}
		if err := ensureBuildCacheDirectory(storageProof.SubstrateCacheDir); err != nil {
			return fmt.Errorf("prepare substrate cache: %w", err)
		}
		if err := ensureBuildCacheDirectory(storageProof.ArtifactCacheDir); err != nil {
			return fmt.Errorf("prepare Artifact cache: %w", err)
		}
		substrateCacheDir = storageProof.SubstrateCacheDir
		artifactCacheDir = storageProof.ArtifactCacheDir
	}
	imagesDir := cfg.ImagesDir
	if imagesDir == "" {
		imagesDir = filepath.Join(workDir, "images")
	}
	guestImageDir := filepath.Join(imagesDir, "guest", "out")
	if supportsBuild {
		guestImageDir, err = firecracker.PrepareRuntime(guestImageDir, workDir, serviceID)
		if err != nil {
			return fmt.Errorf("prepare firecracker runtime: %w", err)
		}
		storage := *buildStorageConfig
		storage.RequiredScratchAvailableBytes = uint64(admissionDiskFloorMiB(true, cfg.VMScratchDiskMiB, cfg.WorkerDiskReserveMiB)) * 1024 * 1024
		storageProof, err := workerdaemon.ProveBuildStorage(storage)
		if err != nil {
			return fmt.Errorf("prove build lease storage after runtime activation: %w", err)
		}
		reserveBytes := uint64(firecracker.BootCorpusMaxMiB) * 1024 * 1024
		buildScratchLeaseBytes = min(
			storageProof.Scratch.AvailableBytes,
			storageProof.Scratch.CapacityBytes-reserveBytes,
		)
	}
	rootfsPath := filepath.Join(guestImageDir, "rootfs.ext4")
	connectorConfig := networkConfig
	connectorConfig.FirecrackerPath = cfg.FirecrackerPath
	connectorConfig.JailerPath = cfg.JailerPath
	connectorConfig.JailerNumaNode = cfg.JailerNumaNode
	connectorConfig.JailerChrootBaseDir = cfg.JailerChrootDir
	connectorConfig.CgroupVersion = cfg.CgroupVersion
	connectorConfig.KernelPath = filepath.Join(guestImageDir, "vmlinuz")
	connectorConfig.InitramfsPath = filepath.Join(guestImageDir, "initramfs")
	connectorConfig.RootfsPath = rootfsPath
	connectorConfig.RuntimeArtifactsPath = filepath.Join(guestImageDir, "runtime-artifacts.json")
	connectorConfig.VCPUCount = cfg.VMVCPUCount
	connectorConfig.MemoryMiB = cfg.VMMemoryMiB
	connectorConfig.ScratchDiskMiB = cfg.VMScratchDiskMiB
	connectorConfig.InitTimeout = cfg.VMInitTimeout
	connectorConfig.HealthTimeout = cfg.VMHealthTimeout
	connectorConfig.HealthAttemptTimeout = cfg.VMHealthAttemptTimeout
	connector, err := firecracker.NewConnector(connectorConfig)
	if err != nil {
		return fmt.Errorf("configure firecracker connector: %w", err)
	}
	if err := connector.Preflight(ctx); err != nil {
		return fmt.Errorf("firecracker worker preflight: %w", err)
	}
	runtimeCapabilities, err := connector.RuntimeCapabilities()
	if err != nil {
		return fmt.Errorf("inspect firecracker runtime: %w", err)
	}
	runtimeArchitecture, err := deployment.RuntimeArchitectureFromGo(runtimeCapabilities.Arch)
	if err != nil {
		return fmt.Errorf("normalize firecracker runtime architecture: %w", err)
	}
	runtimeIdentity := runtimeid.Selector{
		Arch:            string(runtimeArchitecture),
		ABI:             runtimeCapabilities.ABI,
		KernelDigest:    runtimeCapabilities.KernelDigest,
		InitramfsDigest: runtimeCapabilities.InitramfsDigest,
		RootfsDigest:    runtimeCapabilities.RootfsDigest,
		NetworkABI:      runtimeCapabilities.NetworkABI,
	}
	runtimeIdentity.ID, err = runtimeid.Digest(runtimeIdentity)
	if err != nil {
		return fmt.Errorf("derive normalized firecracker runtime identity: %w", err)
	}
	runtimeScratch := filepath.Join(workDir, "tmp", "runtime")
	if err := os.MkdirAll(runtimeScratch, 0o700); err != nil {
		return fmt.Errorf("create Runtime scratch: %w", err)
	}
	if supportsRun || supportsBuild {
		platformStore, err = cas.NewImmutableS3(
			ctx,
			cfg.PlatformStoreURI,
			cas.WithS3TempDir(runtimeScratch),
		)
		if err != nil {
			return fmt.Errorf("configure Platform Artifact store: %w", err)
		}
	}
	if supportsBuild {
		if err := validateWorkerStores(cfg); err != nil {
			return err
		}
		squashfsEncoder, err = deployment.FindEncoder()
		if err != nil {
			return fmt.Errorf("resolve SquashFS encoder: %w", err)
		}
		buildPolicy, err = deployment.LoadBuildPolicy(cfg.BuildPolicyPath)
		if err != nil {
			return fmt.Errorf("load build policy: %w", err)
		}
	}
	var platformAcquirer workerdaemon.PlatformAcquirer
	if supportsBuild {
		acquisitionWorkDir := filepath.Join(workDir, "platform-acquisition")
		if err := ensurePrivateDirectory(acquisitionWorkDir); err != nil {
			return fmt.Errorf("prepare Platform acquisition work directory: %w", err)
		}
		gpgv, err := requireExecutable("gpgv")
		if err != nil {
			return fmt.Errorf("resolve GPG verifier: %w", err)
		}
		xz, err := requireExecutable("xz")
		if err != nil {
			return fmt.Errorf("resolve XZ decoder: %w", err)
		}
		patchelf, err := requireExecutable("patchelf")
		if err != nil {
			return fmt.Errorf("resolve ELF patcher: %w", err)
		}
		workerExecutable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve Worker executable: %w", err)
		}
		platformAcquirer = workerdaemon.PlatformAcquisitionProcess{
			BuildPolicyPath:  cfg.BuildPolicyPath,
			Encoder:          squashfsEncoder,
			Executable:       workerExecutable,
			GPGV:             gpgv,
			Patchelf:         patchelf,
			PlatformStoreURI: cfg.PlatformStoreURI,
			UnitCgroupRoot:   verifierCgroupRoot,
			WorkDir:          acquisitionWorkDir,
			XZ:               xz,
		}
	}
	store, err := cas.NewS3(ctx, cfg.CASURI, cas.WithS3TempDir(filepath.Join(workDir, "tmp", "cas")))
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	vmResources, err := resolveVMResources(cfg, supportsRun, supportsBuild)
	if err != nil {
		return err
	}
	runtimeStartLimit := max(int(cfg.WorkerRuntimeStarts), int(cfg.WorkerBuildExecutors))
	runtimeConnector, err := vm.NewStartLimiter(connector, runtimeStartLimit)
	if err != nil {
		return fmt.Errorf("configure host runtime start limit: %w", err)
	}
	var imageBuilder imageworker.Builder
	if supportsBuild {
		imageBuildWorkDir := filepath.Join(workDir, "image-builds")
		if err := ensurePrivateDirectory(imageBuildWorkDir); err != nil {
			return fmt.Errorf("prepare Workspace image build directory: %w", err)
		}
		imageControlPlane := workerImageControlPlane{client: controlPlaneClient}
		var cacheCredentials imageworker.CacheCredentialFetcher
		if cfg.ImageCache != nil {
			awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
			if err != nil {
				return fmt.Errorf("load Workspace image cache AWS configuration: %w", err)
			}
			cacheConfig := imagecacheecr.Config{
				RegistryAuthority:   cfg.ImageCache.RegistryAuthority,
				RepositoryPrefix:    cfg.ImageCache.RepositoryPrefix,
				CacheRoleARN:        cfg.ImageCache.CacheRoleARN,
				RepositoryARNPrefix: cfg.ImageCache.RepositoryARNPrefix,
			}
			provider, err := imagecacheecr.NewCredentialProvider(
				cacheConfig,
				sts.NewFromConfig(awsCfg),
				imagecacheecr.NewTokenClientFactory(awsCfg),
			)
			if err != nil {
				return fmt.Errorf("configure Workspace image cache credentials: %w", err)
			}
			cacheCredentials = workerImageCacheCredentials{provider: provider}
		}
		imageBuilder = imageworker.VMEngine{
			Connector: runtimeConnector, Admission: imageControlPlane, Credentials: imageControlPlane,
			Cache: cacheCredentials, Completion: imageControlPlane, WorkDir: imageBuildWorkDir,
		}
	}
	hostDiskMiB, err := advertisedWorkerDiskMiB(workDir, cfg.WorkerDiskMiB, cfg.WorkerDiskReserveMiB)
	if err != nil {
		return fmt.Errorf("inspect worker disk capacity: %w", err)
	}
	substrateCacheMaxBytes, artifactCacheMaxBytes := workerCacheBudgetsBytes(cfg.SubstrateCacheMaxMiB, cfg.ArtifactCacheMaxMiB, hostDiskMiB)
	diskCapacity, err := compute.PartitionWorkerDiskCapacity(hostDiskMiB, vmResources.DiskMiB, substrateCacheMaxBytes+artifactCacheMaxBytes)
	if err != nil {
		return fmt.Errorf("partition worker physical disk capacity: %w", err)
	}
	if supportsBuild {
		diskCapacity, err = capGuestEphemeralDiskCapacity(
			diskCapacity,
			uint64(firecracker.BootCorpusMaxMiB)*1024*1024,
			buildScratchLeaseBytes,
		)
		if err != nil {
			return fmt.Errorf("cap worker guest ephemeral disk capacity after runtime activation: %w", err)
		}
	}
	allocatable := compute.ResourceVector{
		MilliCPU:  cfg.WorkerCapacityVCPUs * 1000,
		MemoryMiB: cfg.WorkerCapacityMemoryMiB,
		Slots:     cfg.WorkerExecutionSlots,
	}
	if supportsBuild {
		if !fitsBuildHostCompute(allocatable) {
			return errors.New("build worker allocatable capacity cannot host the fixed build guest")
		}
	}
	workerCapabilities := workerapi.Capabilities{
		ProtocolVersion:           workerapi.CurrentProtocolVersion,
		WorkerVersion:             version.Version,
		RuntimeID:                 runtimeIdentity.ID,
		RuntimeArch:               runtimeIdentity.Arch,
		RuntimeABI:                runtimeCapabilities.ABI,
		KernelDigest:              runtimeCapabilities.KernelDigest,
		InitramfsDigest:           runtimeCapabilities.InitramfsDigest,
		RootfsDigest:              runtimeCapabilities.RootfsDigest,
		NetworkABI:                runtimeCapabilities.NetworkABI,
		MaxVCPUs:                  allocatable.MilliCPU / 1000,
		MaxMemoryMiB:              allocatable.MemoryMiB,
		VMMilliCPU:                vmResources.MilliCPU,
		VMMemoryMiB:               vmResources.MemoryMiB,
		GuestEphemeralDiskBytes:   diskCapacity.HostGuestEphemeralDiskBytes,
		VMGuestEphemeralDiskBytes: diskCapacity.VMGuestEphemeralDiskBytes,
		ExecutionSlotsAvailable:   cfg.WorkerExecutionSlots,
		SupportsRun:               supportsRun,
		SupportsBuild:             supportsBuild,
		MaxBuildExecutors:         cfg.WorkerBuildExecutors,
		MaxRuntimeStarts:          int32(runtimeStartLimit),
		BuildCacheBytes:           substrateCacheMaxBytes,
		ArtifactCacheBytes:        artifactCacheMaxBytes,
	}
	if supportsRun {
		workerCapabilities.SubstrateFormat = substrate.Format
		workerCapabilities.SubstrateBuilderABI = substrate.BuilderABI
		workerCapabilities.SubstrateLayoutABI = substrate.LayoutABI
	}
	hostCapacity, err := capacity.New(capacity.Vector{
		CPUMillis:               workerCapabilities.MaxVCPUs * 1000,
		MemoryBytes:             workerCapabilities.MaxMemoryMiB * 1024 * 1024,
		GuestEphemeralDiskBytes: workerCapabilities.GuestEphemeralDiskBytes,
		VMSlots:                 int64(workerCapabilities.ExecutionSlotsAvailable),
		BuildSlots:              int64(workerCapabilities.MaxBuildExecutors),
	})
	if err != nil {
		return fmt.Errorf("configure worker capacity: %w", err)
	}
	substrateResolver := &substrate.Resolver{
		CacheDir:      substrateCacheDir,
		MkfsExt4Path:  "mkfs.ext4",
		MaxCacheBytes: substrateCacheMaxBytes,
	}
	workspaceMountSessions := executor.NewWorkspaceMountSessions()
	backgroundGate := executor.NewBackgroundWorkGate()
	workspaceMountSessions.BackgroundGate = backgroundGate
	var workspaceMountConnector vm.MaterializingConnector = runtimeConnector
	var preparedRuntimePool *executor.PreparedRuntimePool
	closePreparedRuntime := retryableWorkerCloser{close: func(closeCtx context.Context) error {
		if preparedRuntimePool != nil {
			return preparedRuntimePool.Close(closeCtx)
		}
		return nil
	}}
	defer func() {
		if err := closePreparedRuntime.Close(context.Background()); err != nil {
			log.Warn("prepared runtime pool close failed", "error", err)
		}
	}()
	if cfg.PreparedRuntimePoolSize > 0 {
		preparedRuntimePool = executor.NewPreparedRuntimePool(workspaceMountConnector, store, cfg.PreparedRuntimePoolSize, log)
		preparedRuntimePool.TempDir = filepath.Join(workDir, "tmp")
		preparedRuntimePool.ArtifactCacheDir = artifactCacheDir
		preparedRuntimePool.ArtifactCacheMaxBytes = artifactCacheMaxBytes
		preparedRuntimePool.Substrates = substrateResolver
		preparedRuntimePool.RuntimeSubstrates = controlPlaneClient
		preparedRuntimePool.CheckpointEncryptor = checkpointEncryptor
		preparedRuntimePool.RuntimeInstances = controlPlaneClient
		preparedRuntimePool.BackgroundGate = backgroundGate
		preparedRuntimePool.Capacity = hostCapacity
		preparedRuntimePool.PlatformStore = platformStore
		preparedRuntimePool.RuntimeArchitecture = runtimeArchitecture
		preparedRuntimePool.VerifierCgroupRoot = verifierCgroupRoot
		log.Info("prepared runtime pool enabled", "pool_size", cfg.PreparedRuntimePoolSize)
	}
	runLeaseTasks := executor.ProgramRunner{
		CAS:                 store,
		CheckpointEncryptor: checkpointEncryptor,
		WorkspaceMounts:     workspaceMountSessions,
		TempDir:             filepath.Join(workDir, "tmp"),
	}
	runner, err := workerdaemon.NewRunner(
		controlPlaneClient,
		executor.Executor{
			RunLeases:     controlPlaneClient,
			RunLeaseTasks: runLeaseTasks,
		},
		workerCapabilities,
		workerdaemon.WithCapacity(hostCapacity),
		workerdaemon.WithPollEvery(cfg.PollEvery),
		workerdaemon.WithLogger(log),
		workerdaemon.WithBuildPolicy(buildPolicy),
		workerdaemon.WithPlatformAcquirer(platformAcquirer),
		workerdaemon.WithBuildExecutor(deployment.Builder{
			WorkDir:           workDir,
			CAS:               store,
			PlatformStore:     platformStore,
			Connector:         runtimeConnector,
			RuntimeIdentityID: runtimeIdentity.ID,
			Encoder:           squashfsEncoder,
			Images:            imageBuilder,
		}),
		workerdaemon.WithMaterializer(executor.WorkspaceMaterializer{
			Connector:             workspaceMountConnector,
			CAS:                   store,
			Sessions:              workspaceMountSessions,
			TempDir:               filepath.Join(workDir, "tmp"),
			ArtifactCacheDir:      artifactCacheDir,
			ArtifactCacheMaxBytes: artifactCacheMaxBytes,
			Substrates:            substrateResolver,
			StartupTimeout:        cfg.WorkspaceMountStartupTimeout,
			Log:                   log,
			RuntimePool:           preparedRuntimePool,
			BackgroundGate:        backgroundGate,
			Capacity:              hostCapacity,
		}),
	)
	if err != nil {
		return fmt.Errorf("configure worker: %w", err)
	}
	consumerSpecs := make([]workerdaemon.ConsumerSpec, 0, 4)
	admission := map[string]int{}
	if supportsRun {
		admission["run"] = int(cfg.WorkerExecutionSlots)
		admission["workspace"] = int(cfg.WorkerExecutionSlots)
		consumerSpecs = append(consumerSpecs,
			workerdaemon.ConsumerSpec{Name: "run", Concurrency: int(cfg.WorkerExecutionSlots), Admission: "run", Consumer: workerdaemon.NewRunConsumer(runner)},
			workerdaemon.ConsumerSpec{Name: "workspace", Concurrency: int(cfg.WorkerExecutionSlots), Admission: "workspace", DrainEligible: true, Consumer: workerdaemon.NewWorkspaceConsumer(runner)},
		)
	}
	if supportsBuild {
		admission["build"] = int(cfg.WorkerBuildExecutors)
		consumerSpecs = append(
			consumerSpecs,
			workerdaemon.ConsumerSpec{Name: "platform-acquisition", Concurrency: int(cfg.WorkerBuildExecutors), Admission: "build", Consumer: workerdaemon.NewPlatformAcquisitionConsumer(runner)},
			workerdaemon.ConsumerSpec{Name: "build", Concurrency: int(cfg.WorkerBuildExecutors), Admission: "build", Consumer: workerdaemon.NewBuildConsumer(runner)},
		)
	}
	background := make([]workerdaemon.BackgroundSpec, 0, 1)
	if supportsRun && preparedRuntimePool != nil {
		background = append(background, workerdaemon.BackgroundSpec{Name: "runtime-controller", DrainEligible: true, Run: func(runCtx context.Context) error {
			return preparedRuntimePool.ReconcileDesiredRuntimes(runCtx, controlPlaneClient)
		}})
	}
	hardAdmission, err := workerdaemon.NewHardAdmission(workerdaemon.HardAdmissionConfig{
		Probe: workerdaemon.SystemHostHealthProbe{
			WorkDir: workDir, CgroupVersion: cfg.CgroupVersion, FirecrackerPath: cfg.FirecrackerPath,
		},
		DiskFloorBytes:   admissionDiskFloorMiB(supportsBuild, cfg.VMScratchDiskMiB, cfg.WorkerDiskReserveMiB) * 1024 * 1024,
		FDHeadroom:       256,
		RuntimeSlotCount: cfg.WorkerExecutionSlots,
		DatapathHealth:   connector.DatapathHealth,
	})
	if err != nil {
		return fmt.Errorf("configure worker hard admission: %w", err)
	}
	supervisor, err := workerdaemon.New(workerdaemon.Config{
		ControlPlane: controlPlaneClient, Capabilities: workerCapabilities, Consumers: consumerSpecs, Admission: admission,
		Background: background, PollEvery: cfg.PollEvery,
		AdmissionEvaluator: hardAdmission, Log: log,
		Recover: func(recoveryCtx context.Context) (workerdaemon.RecoveryEvidence, error) {
			var evidence workerdaemon.RecoveryEvidence
			if startupRecovery != nil {
				evidence = *startupRecovery
			} else {
				var err error
				evidence, err = workerdaemon.RecoverLocalVMState(recoveryCtx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim)
				if err != nil {
					return evidence, err
				}
			}
			if len(evidence.Quarantined) != len(evidence.QuarantinedOwners) {
				return evidence, errors.New("startup recovery found VM residue without exact ownership")
			}
			for _, owner := range evidence.QuarantinedOwners {
				if owner.Kind != vm.OwnerRuntime {
					continue
				}
				created, err := hostCapacity.Reserve(
					capacity.Key{Kind: "quarantine", Epoch: 1, ID: owner.ID},
					capacity.Vector{
						CPUMillis:               workerCapabilities.VMMilliCPU,
						MemoryBytes:             workerCapabilities.VMMemoryMiB * 1024 * 1024,
						GuestEphemeralDiskBytes: workerCapabilities.VMGuestEphemeralDiskBytes,
						VMSlots:                 1,
					},
				)
				if err != nil {
					return evidence, fmt.Errorf("reserve quarantined runtime capacity: %w", err)
				}
				if !created {
					return evidence, errors.New("quarantined runtime is already reserved")
				}
			}
			return evidence, nil
		},
		FinalizeDrain: func(finalizeCtx context.Context) (workerdaemon.RecoveryEvidence, error) {
			if err := closePreparedRuntime.Close(finalizeCtx); err != nil {
				return workerdaemon.RecoveryEvidence{}, fmt.Errorf("close prepared runtime pool: %w", err)
			}
			first, err := workerdaemon.RecoverLocalVMState(finalizeCtx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim)
			if err != nil {
				return workerdaemon.RecoveryEvidence{}, err
			}
			if len(first.Quarantined) != 0 || len(first.QuarantineErrors) != 0 {
				return first, nil
			}
			// The first pass reclaims any residue. A second complete inventory is
			// the proof submitted to control and therefore must be empty.
			return workerdaemon.RecoverLocalVMState(finalizeCtx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim)
		},
		DrainCompleted: func(status workerapi.StatusResponse) error {
			return writeDrainCompleteMarker(workDir, status.WorkerInstanceID)
		},
	})
	if err != nil {
		return fmt.Errorf("configure worker supervisor: %w", err)
	}
	if preparedRuntimePool != nil {
		preparedRuntimePool.AdmitRuntimeStart = supervisor.AdmitRuntimeStart
	}
	log.Info("helmr worker listening", "controlplane_url", cfg.ControlPlaneURL, "worker_instance_id", workerCredential.WorkerInstanceID)
	if err := supervisor.Run(ctx); err != nil && err != context.Canceled {
		return err
	}
	return nil
}

func resolveVMResources(cfg config.Worker, supportsRun bool, supportsBuild bool) (compute.ResourceVector, error) {
	resources := compute.ResourceVector{
		MilliCPU:  cfg.VMVCPUCount * 1000,
		MemoryMiB: cfg.VMMemoryMiB,
		DiskMiB:   cfg.VMScratchDiskMiB,
		Slots:     1,
	}
	if supportsBuild && !resources.Fits(compute.ImageBuildGuestResources()) {
		return compute.ResourceVector{}, errors.New("configured VM shape cannot host the fixed image-build guest")
	}
	if supportsBuild && !supportsRun {
		return compute.ImageBuildGuestResources(), nil
	}
	return resources, nil
}

func requireExecutable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable regular file", path)
	}
	return path, nil
}

func fitsBuildHostCompute(resources compute.ResourceVector) bool {
	envelope := compute.BuildEnvelopeResources()
	envelope.DiskMiB = 0
	return resources.Fits(envelope)
}

func validateWorkerStores(cfg config.Worker) error {
	if err := cas.ValidateDistinctS3Stores(
		cfg.CASURI,
		cfg.PlatformStoreURI,
	); err != nil {
		return fmt.Errorf(
			"validate ordinary CAS and Platform Artifact store: %w",
			err,
		)
	}
	return nil
}

func ensureBuildCacheDirectory(path string) error {
	err := os.Mkdir(path, 0o750)
	if err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is not a cache directory", path)
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	err := os.Mkdir(path, 0o700)
	if err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%q is not a private directory", path)
	}
	return nil
}

type retryableWorkerCloser struct {
	mu     sync.Mutex
	close  func(context.Context) error
	closed bool
}

func (c *retryableWorkerCloser) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if c.close == nil {
		c.closed = true
		return nil
	}
	if err := c.close(ctx); err != nil {
		return err
	}
	c.closed = true
	return nil
}
