//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// All paths and storage prefixes belong to an explicitly authorized disposable
// operation. Functional cases use the ordinary worker; performance artifacts
// explicitly instrument its storage client. Enrollment, physical qualification,
// allocation and cleanup use their production owners in both cases.
type nativeExecutionConfig struct {
	Performance                                                                             *nativePerformanceConfig
	RestartPublicationOwner                                                                 bool
	Worker, Node, Driver, Model, Bundle, RuntimeDescriptor, Runtime, Evidence, WorkerCgroup string
	CASURI, PlatformURI                                                                     string
	SourceHostID, TargetHostID                                                              string
	WorkerEnv                                                                               map[string]string
}

type nativeExecutionMode string

const (
	nativeExecutionPerformance     nativeExecutionMode = "performance"
	nativeExecutionBaseline        nativeExecutionMode = "baseline"
	nativeExecutionBackground      nativeExecutionMode = "background"
	nativeExecutionCancelSave      nativeExecutionMode = "cancel-save"
	nativeExecutionDeadlineSave    nativeExecutionMode = "deadline-save"
	nativeExecutionProcessLoss     nativeExecutionMode = "process-loss"
	nativeExecutionHealthy         nativeExecutionMode = "healthy"
	nativeExecutionDiscovery       nativeExecutionMode = "session-discovery"
	nativeExecutionSecrets         nativeExecutionMode = "secret-bindings"
	nativeExecutionLongIdle        nativeExecutionMode = "long-idle"
	nativeExecutionKeyMismatch     nativeExecutionMode = "key-mismatch"
	nativeExecutionCheckpointRetry nativeExecutionMode = "checkpoint-retry"
	nativeExecutionCrossHost       nativeExecutionMode = "cross-host"
	nativeExecutionCorruptConfig   nativeExecutionMode = "corrupt-config"
	nativeExecutionMissingMemory   nativeExecutionMode = "missing-memory"
)

func TestWorkerAgentNativeExecution(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionBaseline)
}
func TestWorkerAgentFinalizationDeadline(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionDeadlineSave)
}
func TestWorkerAgentFinalizationCancellation(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionCancelSave)
}
func TestWorkerAgentNativeProcessLoss(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionProcessLoss)
}
func TestWorkerAgentHealthyContinuation(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionHealthy)
}
func TestWorkerAgentSessionDiscoveryContinuation(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionDiscovery)
}
func TestWorkerAgentSecretBindingsContinuation(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionSecrets)
}
func TestWorkerAgentLongIdleContinuation(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionLongIdle)
}
func TestWorkerAgentCheckpointKeyMismatch(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionKeyMismatch)
}
func TestWorkerAgentCrossHostContinuation(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionCrossHost)
}
func TestWorkerAgentCorruptCheckpoint(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionCorruptConfig)
}
func TestWorkerAgentMissingCheckpointMemory(t *testing.T) {
	runWorkerAgentNativeExecution(t, nativeExecutionMissingMemory)
}

func runWorkerAgentNativeExecution(t *testing.T, mode nativeExecutionMode) {
	performanceCase := mode == nativeExecutionPerformance
	backgroundSaves := mode == nativeExecutionBackground
	deadlineSave := mode == nativeExecutionDeadlineSave
	cancelSave := mode == nativeExecutionCancelSave
	crossHost := mode == nativeExecutionCrossHost
	keyMismatch := mode == nativeExecutionKeyMismatch
	checkpointRetry := mode == nativeExecutionCheckpointRetry
	sessionDiscovery := mode == nativeExecutionDiscovery
	secretBindings := mode == nativeExecutionSecrets
	nativeProcessLoss, healthyRAM := mode == nativeExecutionProcessLoss, mode == nativeExecutionHealthy || performanceCase || mode == nativeExecutionLongIdle || keyMismatch || crossHost || checkpointRetry || sessionDiscovery || secretBindings
	checkpointFault := ""
	if mode == nativeExecutionCorruptConfig || mode == nativeExecutionMissingMemory {
		checkpointFault = string(mode)
	}

	if os.Getenv("HELMR_NATIVE_EXECUTION_PROOF") != "1" {
		t.Skip("requires authorized disposable x86 KVM host and isolated storage")
	}
	if os.Getenv("HELMR_TEST_DATABASE_URL") == "" {
		t.Fatal("explicit disposable PostgreSQL URL is required")
	}
	var input nativeExecutionConfig
	raw, err := os.ReadFile(os.Getenv("HELMR_NATIVE_EXECUTION_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	performanceKind := ""
	if performanceCase {
		if input.Performance == nil || (input.Performance.Kind != "no-op" && input.Performance.Kind != "edit-test" && input.Performance.Kind != "dependencies") || input.RestartPublicationOwner {
			t.Fatal("performance case requires one exact workload and no publication-owner restart")
		}
		performanceKind = input.Performance.Kind
		expectedInterval := "1h"
		if input.Performance.Background {
			expectedInterval = "5s"
		}
		if input.WorkerEnv["WORKER_COMPUTER_SAVE_EVERY"] != expectedInterval {
			t.Fatal("performance save interval does not match selected policy")
		}
	} else if input.Performance != nil {
		t.Fatal("performance configuration requires the performance test")
	}
	for _, path := range []string{input.Worker, input.Node, input.Driver, input.Model, input.Bundle, input.RuntimeDescriptor, input.Runtime, input.Evidence, input.WorkerCgroup} {
		if !filepath.IsAbs(path) {
			t.Fatal("fixture paths must be absolute")
		}
	}
	if backgroundSaves {
		interval, err := time.ParseDuration(input.WorkerEnv["WORKER_COMPUTER_SAVE_EVERY"])
		if err != nil || interval != 30*time.Second {
			t.Fatal("background proof requires the fixture's 30-second interval")
		}
	}
	workRoot := filepath.Join(filepath.Dir(input.Evidence), "w")
	if input.WorkerEnv["WORKER_WORK_DIR"] != workRoot || input.WorkerEnv["JAILER_CHROOT_DIR"] != filepath.Join(workRoot, "jailer") || input.WorkerEnv["WORKER_HOST_SECRET_PATH"] != filepath.Join(workRoot, "host-secret.json") {
		t.Fatal("worker state must use the fixture's new owned working directory")
	}
	if _, err := os.Lstat(workRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worker working directory already exists or cannot be inspected")
	}
	if !strings.Contains(input.CASURI, "/_verification/native-execution/") || !strings.Contains(input.PlatformURI, "/_verification/native-execution/") {
		t.Fatal("isolated native execution storage prefixes are required")
	}
	if err := os.Mkdir(input.Evidence, 0700); err != nil {
		t.Fatal(err)
	}

	duration, driverTimeout := 15*time.Minute, 600000
	// Performance includes eight native Turns, ordinary peer/model Saves and
	// two full RAM releases/restores. Keep the full baseline observable without
	// treating the harness deadline as a product latency acceptance threshold.
	if performanceCase {
		duration, driverTimeout = 35*time.Minute, 30*60*1000
	}
	longIdle := mode == nativeExecutionLongIdle
	// Discovery includes sequential interruption, replacement and cancellation before both restores.
	if keyMismatch || sessionDiscovery {
		duration, driverTimeout = 20*time.Minute, 15*60*1000
	}
	if longIdle {
		duration, driverTimeout = 135*time.Minute, 130*60*1000
	}
	if crossHost {
		actual, err := os.ReadFile("/sys/devices/virtual/dmi/id/board_asset_tag")
		if err != nil || input.SourceHostID == "" || strings.TrimSpace(string(actual)) != input.SourceHostID || input.TargetHostID == "" || input.TargetHostID == input.SourceHostID {
			t.Fatal("cross-host source requires exact, distinct physical host identities")
		}
		// Remote publication includes control-plane round trips for the object graph.
		duration, driverTimeout = 50*time.Minute, 2700000
	}
	ctx, cancel := context.WithTimeout(t.Context(), duration)
	defer cancel()
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	var database dbtest.Database
	if crossHost {
		database = openLossDatabase(t, input, keyBytes)
	} else {
		database = dbtest.Open(t)
	}
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	var extraPermissions []auth.Permission
	if secretBindings {
		extraPermissions = []auth.Permission{auth.PermissionComputersCreate, auth.PermissionSecretsWrite}
	}
	env, key, enrollment := seedNativeExecution(t, database.Pool, extraPermissions...)
	platform, err := cass3.NewImmutable(ctx, input.PlatformURI)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRaw, err := os.ReadFile(input.RuntimeDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := artifact.ParseRuntimeDescriptor(runtimeRaw)
	if err != nil {
		t.Fatal(err)
	}
	runtimeFile, err := os.Open(input.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	_, publishErr := platform.Publish(ctx, cas.Descriptor{Digest: runtime.Digest, SizeBytes: runtime.SizeBytes, MediaType: runtime.MediaType}, runtimeFile)
	if err := errors.Join(publishErr, runtimeFile.Close()); err != nil {
		t.Fatal(err)
	}
	cp := startNativeControlPlane(t, cancel, nativeControlPlaneInput{DSN: database.DSN, Key: keyBytes, Config: input})
	defer cp.close()
	handler := cp.proxy
	gate := nativePublicationGate{pool: database.Pool, environment: env, next: handler, workRoot: workRoot, cancelSave: cancelSave, deadlineSave: deadlineSave, backgroundSaves: backgroundSaves, checkpointRetry: checkpointRetry, evidence: input.Evidence}
	if performanceCase {
		gate.performance = &nativePerformanceGate{pool: database.Pool, environment: env, next: handler, degraded: input.Performance.Degraded}
		defer func() {
			if err := gate.performance.retain(filepath.Join(input.Evidence, "performance-storage-windows.json")); err != nil {
				t.Error(err)
			}
		}()
	}
	gate.clientReconnect = !healthyRAM && checkpointFault == ""
	if input.RestartPublicationOwner {
		gate.restart = cp
	}
	defer func() {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		for _, err := range gate.errors {
			t.Logf("publication gate: %v", err)
		}
		proof, err := json.Marshal(gate.observed)
		if err == nil {
			err = os.WriteFile(filepath.Join(input.Evidence, "publication-gates.json"), proof, 0600)
		}
		if err != nil {
			t.Errorf("retain publication gate evidence: %v", err)
		}
	}()
	server := httptest.NewServer(&gate)
	defer server.Close()
	tokenPath := filepath.Join(input.Evidence, "enrollment-token")
	if err := os.WriteFile(tokenPath, []byte(enrollment), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tokenPath)
	workerEnv := make(map[string]string, len(input.WorkerEnv)+8)
	for k, v := range input.WorkerEnv {
		workerEnv[k] = v
	}
	workerEnv["CONTROL_PLANE_URL"], workerEnv["CAS_URI"], workerEnv["PLATFORM_STORE_URI"] = server.URL, input.CASURI, input.PlatformURI
	workerEnv["WORKER_ENROLLMENT_TOKEN_FILE"], workerEnv["WORKER_POOL_NAME"] = tokenPath, "native-proof"
	workerEnv["WORKER_RESOURCE_ID"] = "native-proof-" + uuid.NewV7().String()
	workerEnv["CHECKPOINT_ENCRYPTION_KEY"] = base64.StdEncoding.EncodeToString(keyBytes)
	if crossHost {
		workerEnv["WORKER_RESOURCE_ID"] = input.SourceHostID
	}
	deploymentClient, err := client.New(server.URL, client.WithBearerToken(key))
	if err != nil {
		t.Fatal(err)
	}
	gate.reader = deploymentClient

	worker, err := startNativeWorker(input, workerEnv, "worker.log")
	if err != nil {
		t.Fatal(err)
	}
	var performanceStarted, performanceFinished time.Time
	if performanceCase {
		cp.mu.Lock()
		processIDs := map[string]int{"controlPlane": cp.child.command.Process.Pid, "worker": worker.cmd.Process.Pid, "observerAndDriverParent": os.Getpid()}
		cp.mu.Unlock()
		observeCtx, stopObserving := context.WithCancel(context.Background())
		observed := make(chan error, 1)
		go func() {
			observed <- observeNativePerformanceResources(observeCtx, input, database.Pool, env, processIDs)
		}()
		defer func() {
			stopObserving()
			if err := <-observed; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("performance resource observations: %v", err)
			}
			if !performanceFinished.IsZero() {
				if err := verifyNativeResourceCoverage(filepath.Join(input.Evidence, "performance-resources.jsonl"), performanceStarted, performanceFinished); err != nil {
					t.Errorf("performance resource coverage: %v", err)
				}
			}
			if err := retainNativeRemoteAfterRetirement(input); err != nil {
				t.Errorf("post-retirement remote footprint: %v", err)
			}
			local, err := nativeLocalFootprint(workRoot)
			if err != nil {
				t.Errorf("post-retirement local footprint: %v", err)
				return
			}
			raw, err := json.Marshal(map[string]any{"observedAt": time.Now().UTC(), "local": local})
			if err == nil {
				err = os.WriteFile(filepath.Join(input.Evidence, "performance-local-after-retirement.json"), raw, 0600)
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	var expectedWorkerStop atomic.Bool
	var stopKeyProof context.CancelFunc
	var keyProofDone chan error
	go func() {
		<-worker.done
		if !expectedWorkerStop.Load() {
			cancel()
		}
	}()
	defer func() {
		// Assertions have ended. Failed-driver cancellation must still publish retained
		// cuts through the production handler so ordinary retirement can converge.
		gate.retiring.Store(true)
		retirementTimeout := 60 * time.Second
		if crossHost {
			retirementTimeout = 5 * time.Minute
		}
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), retirementTimeout)
		defer cancelCleanup()
		if err := retireNativeExecution(cleanup, database.Pool, deploymentClient, env); err != nil {
			t.Errorf("fixture retirement: %v; operator audit required", err)
		} else if crossHost {
			if err := os.WriteFile(filepath.Join(input.Evidence, "retirement-complete.json"), []byte(`{"retired":true}`), 0600); err != nil {
				t.Error(err)
			}
		}
		if stopKeyProof != nil {
			stopKeyProof()
			if err := <-keyProofDone; err != nil {
				t.Errorf("checkpoint key proof shutdown: %v", err)
			}
		}
		expectedWorkerStop.Store(true)
		select {
		case <-worker.done:
			if worker.err != nil {
				t.Errorf("worker shutdown: %v", worker.err)
			}
			return
		default:
		}
		_ = worker.cmd.Process.Signal(syscall.SIGTERM)
		shutdown, cancelShutdown := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancelShutdown()
		if err := worker.wait(shutdown); err != nil {
			_ = worker.cmd.Process.Kill()
			<-worker.done
			t.Errorf("worker did not join physical cleanup: %v; operator audit required", err)
		}
	}()
	closedBundle, err := bundle.ReadDirectory(input.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := deploymentClient.PlanDeploymentBundleUploads(ctx, closedBundle.BundleJSON, client.EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, upload := range plan.Uploads {
		path, ok := closedBundle.Objects[upload.Digest]
		if !ok {
			t.Fatal("upload requested an object outside the bundle")
		}
		if err := deploymentClient.UploadDeploymentBundleObject(ctx, upload, path, nil); err != nil {
			t.Fatal(err)
		}
	}
	deployment, err := deploymentClient.FinalizeDeploymentBundle(ctx, api.FinalizeDeploymentBundleRequest{IdempotencyKey: "native-proof", BundleDigest: plan.BundleDigest}, client.EnvironmentScopeOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deploymentClient.PromoteDeployment(ctx, deployment.ID, client.EnvironmentScopeOptions{}); err != nil {
		t.Fatal(err)
	}
	driverCtx, stopDriver := context.WithCancelCause(ctx)
	defer stopDriver(nil)
	checkpointObservations := filepath.Join(input.Evidence, "checkpoint-observations.json")
	if healthyRAM || checkpointFault != "" {
		observeCtx, stopObserving := context.WithCancel(ctx)
		observed := make(chan error, 1)
		go func() {
			observed <- writeNativeCheckpointObservations(observeCtx, database.Pool, env, checkpointObservations)
		}()
		defer func() {
			stopObserving()
			if err := <-observed; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("checkpoint observations: %v", err)
			}
		}()
	}
	if checkpointFault != "" {
		faultCtx, stopFault := context.WithCancel(ctx)
		faultDone := make(chan error, 1)
		go func() {
			err := injectNativeCheckpointFault(faultCtx, database.Pool, env, input.CASURI, checkpointObservations, mode)
			if err != nil {
				stopDriver(err)
			}
			faultDone <- err
		}()
		defer func() {
			stopFault()
			if err := <-faultDone; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("checkpoint fault: %v", err)
			}
		}()
	}

	if keyMismatch {
		var keyCtx context.Context
		keyCtx, stopKeyProof = context.WithCancel(context.WithoutCancel(ctx))
		keyProofDone = make(chan error, 1)
		go func() {
			keyProofDone <- qualifyNativeCheckpointKeyMismatch(keyCtx, database.Pool, env, input, checkpointObservations, worker, &expectedWorkerStop, workerEnv, stopDriver)
		}()
	}

	if crossHost {
		handoffCtx, stopHandoff := context.WithCancel(ctx)
		handoffDone := make(chan error, 1)
		go func() {
			err := handoffNativeCheckpoint(handoffCtx, database.Pool, env, input, checkpointObservations, worker, &expectedWorkerStop, nativeCrossHostBootstrap{SourceHostID: input.SourceHostID, TargetHostID: input.TargetHostID, ControlPlaneURL: server.URL, EnrollmentToken: enrollment, CheckpointKey: workerEnv["CHECKPOINT_ENCRYPTION_KEY"], CASURI: input.CASURI, PlatformURI: input.PlatformURI})
			if err != nil {
				stopDriver(err)
			}
			handoffDone <- err
		}()
		defer func() {
			stopHandoff()
			if err := <-handoffDone; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("cross-host handoff: %v", err)
			}
		}()
	}
	driverConfig, err := json.Marshal(map[string]any{"url": server.URL, "apiKey": key, "modelScript": input.Model, "timeoutMs": driverTimeout, "nativeProcessLoss": nativeProcessLoss, "cancelSave": cancelSave, "deadlineSave": deadlineSave, "evidence": input.Evidence, "healthyRAM": healthyRAM, "performanceKind": performanceKind, "sessionDiscovery": sessionDiscovery, "secretBindings": secretBindings, "longIdle": longIdle, "checkpointKeyMismatch": keyMismatch, "checkpointFault": checkpointFault, "checkpointObservations": checkpointObservations, "backgroundSaves": backgroundSaves, "crossHost": crossHost, "sourceHostId": input.SourceHostID, "targetHostId": input.TargetHostID})
	if err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(input.Evidence, "driver-config.json")
	if err := os.WriteFile(driverPath, driverConfig, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(driverPath)
	driver := exec.CommandContext(driverCtx, input.Node, input.Driver, driverPath)
	driver.WaitDelay = 10 * time.Second
	var result, diagnostic bytes.Buffer
	driver.Stdout, driver.Stderr = &result, &diagnostic
	performanceStarted = time.Now()
	err = driver.Run()
	performanceFinished = time.Now()
	if writeErr := os.WriteFile(filepath.Join(input.Evidence, "driver.log"), diagnostic.Bytes(), 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil {
		if healthyRAM || checkpointFault != "" {
			diagnosticCtx, stopDiagnostic := context.WithTimeout(t.Context(), 5*time.Second)
			var checkpoints []byte
			checkpointErr := database.Pool.QueryRow(diagnosticCtx, `SELECT COALESCE(jsonb_agg(observed),'[]'::jsonb) FROM (
 SELECT jsonb_build_object('checkpointId',p.id,'status',p.status,'createdAt',p.created_at,
  'readyAt',p.ready_at,'terminalEvidence',p.terminal_evidence,'hasManifest',p.manifest IS NOT NULL,
  'runtimeObjectCount',(SELECT count(*) FROM computer_checkpoint_objects o WHERE o.environment_id=p.environment_id AND o.checkpoint_id=p.id),
  'diskSaveId',s.id,'diskSaveStatus',s.status,'diskCapturedAt',s.captured_at,'diskFailureEvidence',s.failure_evidence,
  'sourceLeaseStatus',l.status,'sourceFencedAt',l.fenced_at) observed
 FROM computer_checkpoints p JOIN computer_saves s ON(s.environment_id,s.id)=(p.environment_id,p.disk_save_id)
 JOIN computer_leases l ON(l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.source_lease_epoch)
 WHERE p.environment_id=$1 ORDER BY p.created_at DESC LIMIT 10) recent`, env).Scan(&checkpoints)
			stopDiagnostic()
			if checkpointErr == nil {
				checkpointErr = os.WriteFile(filepath.Join(input.Evidence, "checkpoint-failure.json"), checkpoints, 0600)
			}
			if checkpointErr != nil {
				t.Logf("retain checkpoint failure evidence: %v", checkpointErr)
			}
		}
		diagnosticCtx, stopDiagnostic := context.WithTimeout(t.Context(), 5*time.Second)
		var diagnostics []byte
		diagnosticErr := database.Pool.QueryRow(diagnosticCtx, `SELECT COALESCE(jsonb_agg(to_jsonb(log) ORDER BY id),'[]'::jsonb)
 FROM (SELECT id,command_id,session_id,stream,sequence,encode(data,'base64') AS data_base64
       FROM telemetry_outbox WHERE environment_id=$1 AND kind='data'
         AND (command_id IS NOT NULL OR (session_id IS NOT NULL AND stream='stderr'))
       ORDER BY id DESC LIMIT 100) log`, env).Scan(&diagnostics)
		stopDiagnostic()
		if diagnosticErr == nil {
			diagnosticErr = os.WriteFile(filepath.Join(input.Evidence, "execution-diagnostics.json"), diagnostics, 0600)
		}
		if diagnosticErr != nil {
			t.Logf("retain execution diagnostics: %v", diagnosticErr)
		}
		t.Fatalf("SDK driver: %v; see retained driver/worker logs", err)
	}
	if err := os.WriteFile(filepath.Join(input.Evidence, "result.json"), result.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		ComputerID     string            `json:"computerId"`
		PeerSessionID  string            `json:"peerSessionId"`
		NativeSessions map[string]string `json:"nativeSessions"`
		NativeLosses   []struct {
			Provider        string `json:"provider"`
			SessionID       string `json:"sessionId"`
			LostTurnID      string `json:"lostTurnId"`
			RecoveredTurnID string `json:"recoveredTurnId"`
			HoldID          string `json:"holdId"`
			HoldReason      string `json:"holdReason"`
		} `json:"nativeLosses"`
		Turns []struct {
			ID   string `json:"id"`
			Save string `json:"completionSaveId"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(result.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if checkpointFault != "" {
		if err := verifyNativeCheckpointLoss(ctx, database.Pool, env, result.Bytes(), input.Evidence); err != nil {
			t.Fatal(err)
		}
	}
	if cancelSave {
		if err := verifyNativeFinalizationCancellation(ctx, database.Pool, env, result.Bytes(), input.Evidence); err != nil {
			t.Fatal(err)
		}
	}
	if deadlineSave {
		if err := verifyNativeFinalizationDeadline(ctx, database.Pool, env, result.Bytes(), input.Evidence); err != nil {
			t.Fatal(err)
		}
	}
	if sessionDiscovery {
		if err := verifyNativeSessionControls(ctx, database.Pool, env, result.Bytes(), input.Evidence); err != nil {
			t.Fatal(err)
		}
	}
	expectedTurns := 4
	if healthyRAM {
		expectedTurns = 8
		if err := verifyNativeHealthyContinuation(ctx, database.Pool, env, result.Bytes(), input.Evidence, longIdle, keyMismatch); err != nil {
			t.Fatal(err)
		}
	}
	if crossHost {
		if err := verifyNativeCrossHostContinuation(ctx, database.Pool, env, result.Bytes(), input); err != nil {
			t.Fatal(err)
		}
	}
	if nativeProcessLoss {
		expectedTurns = 6
		if len(receipt.NativeLosses) != 2 {
			t.Fatalf("native loss evidence count: %d", len(receipt.NativeLosses))
		}
		providers := map[string]bool{}
		for _, loss := range receipt.NativeLosses {
			if (loss.Provider != "codex" && loss.Provider != "claude") || providers[loss.Provider] || loss.SessionID != receipt.NativeSessions[loss.Provider] {
				t.Fatalf("native loss identity: %+v", loss)
			}
			providers[loss.Provider] = true
			if loss.HoldReason != "native_continuation_lost" && loss.HoldReason != "native_convergence_failed" {
				t.Fatalf("unexpected native loss hold: %q", loss.HoldReason)
			}
			var valid bool
			err := database.Pool.QueryRow(ctx, `SELECT
 lost.status='interrupted' AND lost.result IS NULL AND lost.completion_save_id IS NULL
 AND recovered.status='completed' AND recovered.process_epoch=lost.process_epoch+1
 AND h.scope='local' AND h.issuer_kind='system' AND h.reason=$6 AND h.released_at IS NOT NULL
 AND recovered.started_at>=h.released_at
 AND old_process.status='stopped' AND old_process.failure_recorded_at IS NOT NULL
 AND old_process.fenced_at IS NOT NULL AND recovered.started_at>=old_process.fenced_at
 AND (SELECT count(*) FROM session_holds other WHERE other.environment_id=lost.environment_id AND other.session_id=lost.session_id AND other.created_at>=lost.started_at)=1
 FROM turns lost JOIN turns recovered ON recovered.environment_id=lost.environment_id AND recovered.session_id=lost.session_id
 JOIN session_holds h ON h.environment_id=lost.environment_id AND h.session_id=lost.session_id
 JOIN session_processes old_process ON (old_process.environment_id,old_process.session_id,old_process.epoch)=(lost.environment_id,lost.session_id,lost.process_epoch)
 WHERE lost.environment_id=$1 AND lost.session_id=$2 AND lost.id=$3 AND recovered.id=$4 AND h.id=$5`,
				env, loss.SessionID, loss.LostTurnID, loss.RecoveredTurnID, loss.HoldID, loss.HoldReason).Scan(&valid)
			if err != nil || !valid {
				t.Fatalf("native loss hold/reconstruction evidence: %v %v", valid, err)
			}
		}
	}
	if len(receipt.Turns) != expectedTurns {
		t.Fatalf("native completion count: %d", len(receipt.Turns))
	}
	var saveTimelines []json.RawMessage
	for _, turn := range receipt.Turns {
		var valid bool
		var saveEvidence json.RawMessage
		err := database.Pool.QueryRow(ctx, `SELECT t.status='completed' AND s.status='published' AND s.turn_id=t.id AND s.computer_id=t.computer_id
   AND octet_length(s.captured_root_digest)=32 AND ((s.root_id IS NOT NULL) <> (s.payload_retired_at IS NOT NULL))
   AND s.capture_evidence IS NOT NULL AND s.publication_evidence IS NOT NULL,
   jsonb_build_object('turnId',t.id,'saveId',s.id,'turnStatus',t.status,'saveStatus',s.status,
    'capturedRootDigest',encode(s.captured_root_digest,'hex'),'rootRetained',s.root_id IS NOT NULL,'payloadRetiredAt',s.payload_retired_at,
    'captureRecorded',s.capture_evidence IS NOT NULL,'publicationRecorded',s.publication_evidence IS NOT NULL)
   FROM turns t JOIN computer_saves s ON (s.environment_id,s.id)=(t.environment_id,t.completion_save_id)
   WHERE t.environment_id=$1 AND t.id=$2 AND s.id=$3 AND t.computer_id=$4`, env, turn.ID, turn.Save, receipt.ComputerID).Scan(&valid, &saveEvidence)
		if err != nil || !valid {
			// Retain the remaining independent acceptance checks after a bad receipt.
			t.Errorf("native Turn %s own-save evidence: %v %v; %s", turn.ID, valid, err, saveEvidence)
		}
		var timeline json.RawMessage
		err = database.Pool.QueryRow(ctx, `WITH save AS (
 SELECT t.id AS turn_id,t.processing_closed_at,t.result_recorded_at,t.terminal_at,
        s.id AS save_id,s.requested_at,s.flush_acknowledged_at,s.captured_at,s.captured_root_digest,s.root_id,s.payload_retired_at
 FROM turns t JOIN computer_saves s ON (s.environment_id,s.id)=(t.environment_id,t.completion_save_id)
 WHERE t.environment_id=$1 AND t.id=$2 AND s.id=$3
), peer AS (
 SELECT e.created_at FROM session_events e,save s
 WHERE e.environment_id=$1 AND e.session_id=$4 AND e.kind='turn.output'
   AND e.created_at>=s.requested_at AND e.created_at<=s.captured_at
), points AS (
 SELECT requested_at AS at FROM save UNION ALL SELECT created_at FROM peer UNION ALL SELECT captured_at FROM save
), gaps AS (SELECT at-lag(at) OVER (ORDER BY at) AS gap FROM points)
SELECT jsonb_build_object('turnId',turn_id,'saveId',save_id,
 'processingClosedAt',processing_closed_at,'resultRecordedAt',result_recorded_at,'terminalAt',terminal_at,
 'requestedAt',requested_at,'flushAcknowledgedAt',flush_acknowledged_at,'capturedAt',captured_at,
 'capturedRootDigest',encode(captured_root_digest,'hex'),'rootRetained',root_id IS NOT NULL,'payloadRetiredAt',payload_retired_at,
 'peerOutputCountWithinCaptureWindow',(SELECT count(*) FROM peer),
 'peerMaxSilentIntervalWithinCaptureWindowMs',(SELECT max(extract(epoch FROM gap)*1000) FROM gaps))
FROM save`, env, turn.ID, turn.Save, receipt.PeerSessionID).Scan(&timeline)
		if err != nil {
			t.Fatalf("native save timeline: %v", err)
		}
		saveTimelines = append(saveTimelines, timeline)
	}
	// The two capture timestamps are the CP's single receipt-observation time,
	// not distinct physical phase clocks. Worker phase logs measure flush/cut.
	// Peer gaps are clipped to the requested-to-captured window; no latency
	// threshold or sub-sample scheduling guarantee is inferred from this sample.
	timelineJSON, err := json.Marshal(saveTimelines)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.Evidence, "save-timelines.json"), timelineJSON, 0600); err != nil {
		t.Fatal(err)
	}
	var mcpTurns int
	err = database.Pool.QueryRow(ctx, `SELECT count(*) FROM turns t JOIN sessions source ON (source.environment_id,source.id)=(t.environment_id,t.caller_id) JOIN agents a ON (a.environment_id,a.id)=(source.environment_id,source.agent_id)
  WHERE t.environment_id=$1 AND t.session_id=$2 AND t.caller_kind='session' AND t.admission_method='enqueue' AND t.origin_turn_id IS NULL AND ((a.name='codex' AND source.id=$3) OR (a.name='claude' AND source.id=$4))
  AND (t.retry_key IN (a.name||'-1',a.name||'-3') OR ($5 AND t.retry_key=a.name||'-5') OR ($6 AND t.retry_key IN (a.name||'-5',a.name||'-7')))`, env, receipt.PeerSessionID, receipt.NativeSessions["codex"], receipt.NativeSessions["claude"], nativeProcessLoss || cancelSave, healthyRAM).Scan(&mcpTurns)
	expectedSaves := expectedTurns
	if cancelSave {
		expectedSaves += 2
	}
	expectedMCPTurns := expectedSaves
	if performanceCase {
		expectedMCPTurns = 0
	}
	if err != nil || mcpTurns != expectedMCPTurns {
		t.Fatalf("native MCP peer Turns=%d: %v", mcpTurns, err)
	}
	if checkpointRetry {
		if err := gate.verifyCheckpointReadRetry(ctx); err != nil {
			t.Fatalf("checkpoint read retry: %v", err)
		}
	}
	if input.RestartPublicationOwner {
		saveIDs := make([]string, 0, len(receipt.Turns))
		for _, turn := range receipt.Turns {
			saveIDs = append(saveIDs, turn.Save)
		}
		retryCtx, stopRetryWait := context.WithTimeout(ctx, 15*time.Second)
		err := gate.awaitReconciliation(retryCtx, saveIDs)
		stopRetryWait()
		if err != nil {
			t.Fatalf("publication restart reconciliation: %v", err)
		}
	}
	if deadlineSave {
		expectedSaves++
	}
	if performanceCase {
		saves := make([]string, 0, len(receipt.Turns))
		for _, turn := range receipt.Turns {
			saves = append(saves, turn.Save)
		}
		if err := gate.performance.verify(saves); err != nil {
			t.Fatal(err)
		}
		if err := retainNativePerformance(ctx, database.Pool, env, result.Bytes(), input); err != nil {
			t.Fatal(err)
		}
		if err := gate.performance.retainStorageObservations(input, saves); err != nil {
			t.Fatal(err)
		}
		return
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if backgroundSaves && len(gate.background) != 3 {
		t.Fatalf("background publication evidence: %d saves", len(gate.background))
	}
	if len(gate.errors) != 0 || len(gate.observed) != expectedSaves {
		t.Fatalf("publication gate evidence: %d saves, %v", len(gate.observed), gate.errors)
	}

}

// Requests retirement through public owners even if the driver failed before
// returning a receipt. SQL only discovers objects in this disposable Environment
// and observes physical acknowledgements; it never manufactures a fence.
func retireNativeExecution(ctx context.Context, pool *pgxpool.Pool, c *client.Client, env uuid.UUID) error {
	ids := func(query string) ([]string, error) {
		rows, err := pool.Query(ctx, query, env)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[string])
	}
	sessions, err := ids(`SELECT id::text FROM sessions WHERE environment_id=$1`)
	if err != nil {
		return err
	}
	for _, id := range sessions {
		if _, err := c.CancelSession(ctx, id, api.CancelSessionRequest{IdempotencyKey: "fixture-retire-" + id}, client.EnvironmentScopeOptions{}); err != nil {
			return err
		}
	}
	commands, err := ids(`SELECT id::text FROM computer_commands WHERE environment_id=$1`)
	if err != nil {
		return err
	}
	for _, id := range commands {
		if _, err := c.CancelCommand(ctx, id, client.ComputerScopeOptions{}); err != nil {
			return err
		}
	}
	computers, err := ids(`SELECT id::text FROM computers WHERE environment_id=$1`)
	if err != nil {
		return err
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for _, id := range computers {
		for {
			_, err := c.DeleteComputer(ctx, id, api.DeleteComputerRequest{IdempotencyKey: "fixture-retire-" + id}, client.ComputerScopeOptions{})
			if err == nil {
				break
			}
			var apiErr *httpclient.Error
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || apiErr.Code != "computer_busy" {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			}
		}
	}
	for {
		var stopped bool
		err := pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND fenced_at IS NULL)
      AND NOT EXISTS(SELECT 1 FROM computer_preparations WHERE environment_id=$1 AND worker_host_id IS NOT NULL AND fenced_at IS NULL)`, env).Scan(&stopped)
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func nativeExecutionEnv(overrides map[string]string) []string {
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := overrides[key]; !replace {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func seedNativeExecution(t *testing.T, pool *pgxpool.Pool, extraPermissions ...auth.Permission) (uuid.UUID, string, string) {
	t.Helper()
	org, user, project, env := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), pool, `
 INSERT INTO regions(id,display_name) VALUES('native-proof','Native proof');
 INSERT INTO organizations(id,name,slug) VALUES($1,'Native proof','native-proof');
 INSERT INTO users(id,display_name) VALUES($2,'Fixture');
 INSERT INTO org_members(org_id,user_id,role) VALUES($1,$2,'owner');
 INSERT INTO projects(id,org_id,default_region_id,slug,name) VALUES($3,$1,'native-proof','native-proof','Native proof');
 INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) VALUES('until_environment_deletion',$4,$1,$3,'native-proof','Native proof','#112233');
 UPDATE environments SET max_outstanding_admissions=128,max_causal_depth=8,max_resident_computers=2,max_cpu_millis=4000,max_memory_bytes=17179869184,max_reserved_storage_bytes=137438953472,preparation_timeout_ms=600000,admission_rate_per_second=100,admission_burst=100,admission_tokens=100,admission_refilled_at=clock_timestamp() WHERE id=$4;`, pgx.QueryExecModeSimpleProtocol, org, user, project, env)
	issued, err := identity.IssueAPIKey(t.Context(), db.New(pool), auth.Principal{OrgID: org, UserID: user, Kind: auth.PrincipalKindSession, Role: auth.RoleOwner}, auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: env.String()}, identity.APIKeyInput{Name: "native proof", Permissions: append([]auth.Permission{auth.PermissionDeploymentsWrite, auth.PermissionAgentsStart, auth.PermissionSessionsRead, auth.PermissionSessionsSend, auth.PermissionSessionsClose, auth.PermissionSessionsCancel, auth.PermissionSessionsResume, auth.PermissionComputersRead, auth.PermissionComputersDelete, auth.PermissionComputerCommandCreate, auth.PermissionAsksRespond}, extraPermissions...)})
	if err != nil {
		t.Fatal(err)
	}
	group, err := workergroup.CreateGroup(t.Context(), pool, workergroup.GroupInput{RegionID: "native-proof", Name: "native-proof"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := workergroup.CreatePool(t.Context(), pool, pgvalue.MustUUIDValue(group.Group.ID), "native-proof", group.Group.ClaimVersion); err != nil {
		t.Fatal(err)
	}
	return env, issued.Raw, group.EnrollmentToken
}

type nativePublicationGate struct {
	performance         *nativePerformanceGate
	retiring            atomic.Bool
	clientReconnect     bool
	checkpointRetry     bool
	checkpointReadRetry *nativeCheckpointReadRetry
	backgroundSaves     bool
	background          []json.RawMessage
	deadlineSave        bool
	cancelSave          bool
	evidence            string
	workRoot            string
	recovery            map[string]*nativeEarlyRecovery
	restartMu           sync.Mutex
	restart             *nativeControlPlaneProcess
	pool                *pgxpool.Pool
	environment         uuid.UUID
	next                http.Handler
	reader              *client.Client
	mu                  sync.Mutex
	errors              []error
	observed            map[string]nativePublicationObservation
}

type nativePublicationObservation struct {
	DeadlineAttempted                                                             bool
	Deadline                                                                      *nativeFinalizationDeadline
	CancellationAttempted                                                         bool
	Cancellation                                                                  *nativeFinalizationCancellation
	ReconciledAt                                                                  time.Time
	EarlyRecovery                                                                 *nativeEarlyRecovery
	TurnSequence                                                                  int64
	RequestDigest                                                                 string
	Restart                                                                       *nativePublicationRestart
	PeerCounterBefore, PeerCounterAfter                                           int64
	PublicOutputRecords                                                           int
	QueuedFollowup                                                                bool
	PublicationHoldMs                                                             float64
	PublicationEnteredAt, PublicationFirstForwardedAt, PublicationFirstReturnedAt time.Time
	PublicationLastReturnedAt                                                     time.Time
	PublicationAttempts                                                           int
}

func TestNativePublicationTimingRetainsFirstAttempt(t *testing.T) {
	save := uuid.NewV7().String()
	attempt := 0
	gate := nativePublicationGate{
		observed: map[string]nativePublicationObservation{save: {PublicationEnteredAt: time.Now().UTC()}},
		next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempt++
			if attempt == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	body, err := json.Marshal(workerapi.AgentSavePublication{Save: workerapi.AgentSave{SaveID: save}})
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	gate.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/publish", bytes.NewReader(body)))
	before := gate.observed[save]
	if first.Code != http.StatusServiceUnavailable || before.PublicationAttempts != 1 || before.PublicationFirstForwardedAt.IsZero() || before.PublicationFirstReturnedAt.IsZero() {
		t.Fatalf("first failed attempt was not retained: %+v", before)
	}
	second := httptest.NewRecorder()
	gate.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/publish", bytes.NewReader(body)))
	after := gate.observed[save]
	if second.Code != http.StatusOK || after.PublicationAttempts != 2 ||
		after.PublicationFirstForwardedAt != before.PublicationFirstForwardedAt ||
		after.PublicationFirstReturnedAt != before.PublicationFirstReturnedAt ||
		after.PublicationEnteredAt != before.PublicationEnteredAt ||
		after.PublicationLastReturnedAt.Before(before.PublicationFirstReturnedAt) {
		t.Fatalf("retry replaced the first attempt or lost its completion: %+v", after)
	}
}

func TestNativePublicationRetirementPreservesFailureAndForwards(t *testing.T) {
	original := errors.New("original assertion failure")
	gate := nativePublicationGate{errors: []error{original}}
	body := []byte(`{"save":{"save_id":"retained-cut"}}`)
	calls := 0
	gate.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("retirement changed publication: %s %v", got, err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	gate.retiring.Store(true)
	response := httptest.NewRecorder()
	gate.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/publish", bytes.NewReader(body)))
	if calls != 1 || response.Code != http.StatusNoContent || len(gate.errors) != 1 || gate.errors[0] != original || len(gate.observed) != 0 {
		t.Fatalf("retirement lost failure, asserted publication or blocked production: calls=%d status=%d errors=%v", calls, response.Code, gate.errors)
	}
}

func (g *nativePublicationGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.retiring.Load() {
		g.next.ServeHTTP(w, r)
		return
	}
	if g.performance != nil {
		g.performance.ServeHTTP(w, r)
		return
	}
	if g.checkpointRetry && r.URL.Path == "/worker/v1/agent-computers/checkpoint/read" {
		if err := g.serveCheckpointReadRetry(w, r); err != nil {
			g.mu.Lock()
			g.errors = append(g.errors, err)
			g.mu.Unlock()
			http.Error(w, "checkpoint retry proof failed", http.StatusInternalServerError)
		}
		return
	}

	if g.restart != nil && (r.URL.Path == "/worker/v1/computer-saves/capture" || r.URL.Path == "/worker/v1/computer-saves/objects/certify") {
		if err := g.serveEarlyRecovery(w, r); err != nil {
			g.mu.Lock()
			g.errors = append(g.errors, err)
			g.mu.Unlock()
			http.Error(w, "early save recovery proof failed", http.StatusInternalServerError)
		}
		return
	}

	var save, requestDigest string
	firstPublication := false
	if r.URL.Path == "/worker/v1/computer-saves/publish" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request workerapi.AgentSavePublication
			if err = json.Unmarshal(body, &request); err == nil {
				save = request.Save.SaveID
				requestDigest = fmt.Sprintf("%x", sha256.Sum256(body))
				err = g.observe(r.Context(), request.Save.SaveID)
				if err == nil && g.cancelSave {
					err = g.holdNativeCancellation(r.Context(), request.Save.SaveID)
				}
				if err == nil && g.deadlineSave {
					err = g.holdNativeDeadline(r.Context(), request.Save.SaveID)
				}
			}
		}
		if err != nil {
			g.mu.Lock()
			g.errors = append(g.errors, err)
			g.mu.Unlock()
			http.Error(w, "fixture publication gate failed", http.StatusInternalServerError)
			return
		}
	}
	if save != "" {
		g.mu.Lock()
		if observation, ok := g.observed[save]; ok {
			if observation.RequestDigest != "" && observation.RequestDigest != requestDigest {
				g.errors = append(g.errors, errors.New("publication retry changed its exact request"))
				g.mu.Unlock()
				http.Error(w, "publication identity changed", http.StatusInternalServerError)
				return
			}
			observation.RequestDigest = requestDigest
			observation.PublicationAttempts++
			firstPublication = observation.PublicationAttempts == 1
			if observation.PublicationFirstForwardedAt.IsZero() && !(firstPublication && g.restart != nil && observation.TurnSequence == 1) {
				observation.PublicationFirstForwardedAt = time.Now().UTC()
			}
			g.observed[save] = observation
		}
		g.mu.Unlock()
	}
	if firstPublication && g.restart != nil {
		if err := g.restartPublication(w, r, save); err != nil {
			g.mu.Lock()
			g.errors = append(g.errors, err)
			g.mu.Unlock()
			http.Error(w, "publication restart proof failed", http.StatusInternalServerError)
		}
	} else if save != "" {
		response := httptest.NewRecorder()
		g.next.ServeHTTP(response, r)
		if response.Code == http.StatusNoContent && g.backgroundSaves {
			if err := g.observeBackgroundPublication(r.Context(), save); err != nil {
				g.mu.Lock()
				g.errors = append(g.errors, err)
				g.mu.Unlock()
			}
		}
		if response.Code == http.StatusNoContent && g.cancelSave {
			if err := g.observeNativeCancelledPublication(r.Context(), save); err != nil {
				g.mu.Lock()
				g.errors = append(g.errors, err)
				g.mu.Unlock()
			}
		}
		if response.Code == http.StatusNoContent && g.deadlineSave {
			if err := g.observeNativeDeadlinePublication(r.Context(), save); err != nil {
				g.mu.Lock()
				g.errors = append(g.errors, err)
				g.mu.Unlock()
			}
		}
		for name, values := range response.Header() {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.Code)
		_, _ = io.Copy(w, response.Body)
		if response.Code == http.StatusNoContent {
			g.mu.Lock()
			if observation, ok := g.observed[save]; ok && observation.Restart != nil {
				observation.ReconciledAt = time.Now().UTC()
				g.observed[save] = observation
			}
			g.mu.Unlock()
		}
	} else {
		g.next.ServeHTTP(w, r)
	}
	if save != "" {
		g.mu.Lock()
		if observation, ok := g.observed[save]; ok {
			observation.PublicationLastReturnedAt = time.Now().UTC()
			if firstPublication {
				// A returned request is not a claim that publication committed.
				observation.PublicationFirstReturnedAt = observation.PublicationLastReturnedAt
			}
			g.observed[save] = observation
		}
		g.mu.Unlock()
	}
}
func (g *nativePublicationGate) observe(ctx context.Context, save string) error {
	g.mu.Lock()
	_, observed := g.observed[save]
	g.mu.Unlock()
	if observed {
		return nil
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var turnSave bool
	if err := g.pool.QueryRow(ctx, `SELECT turn_id IS NOT NULL FROM computer_saves WHERE environment_id=$1 AND id=$2`, g.environment, save).Scan(&turnSave); err != nil {
		return err
	}
	if !turnSave {
		return nil
	}
	var native, valid bool
	var peer, turn, session uuid.UUID
	var sequence int64
	var result json.RawMessage
	err := g.pool.QueryRow(ctx, `SELECT a.name IN ('codex','claude'), t.status='finalizing' AND t.completion_save_id IS NULL AND s.status='captured' AND s.root_id IS NULL, t.id, t.session_id, t.seq, t.result
  FROM computer_saves s JOIN turns t ON (t.environment_id,t.id)=(s.environment_id,s.turn_id) JOIN sessions se ON(se.environment_id,se.id)=(t.environment_id,t.session_id) JOIN agents a ON(a.environment_id,a.id)=(se.environment_id,se.agent_id) WHERE s.environment_id=$1 AND s.id=$2`, g.environment, save).Scan(&native, &valid, &turn, &session, &sequence, &result)
	if err != nil {
		return err
	}
	if !native {
		return nil
	}
	if !valid {
		return errors.New("native save was not captured and finalizing before publication")
	}
	g.restartMu.Lock()
	var early *nativeEarlyRecovery
	if recovery := g.recovery[save]; recovery != nil {
		snapshot := *recovery
		early = &snapshot
	}
	g.restartMu.Unlock()
	observation := nativePublicationObservation{TurnSequence: sequence, EarlyRecovery: early}
	var expected any
	if err := json.Unmarshal(result, &expected); err != nil {
		return err
	}
	resultReadable := false
	for after := int64(0); ; {
		page, err := g.reader.ReadSessionEvents(ctx, session.String(), client.SessionEventReadOptions{After: after, Limit: 100})
		if err != nil {
			return err
		}
		for _, event := range page.Records {
			if event.TurnID != nil && *event.TurnID == turn.String() && event.Kind == "turn.output" {
				observation.PublicOutputRecords++
				var content []struct {
					Type  string `json:"type"`
					Value any    `json:"value"`
				}
				if err := json.Unmarshal(event.Data, &content); err != nil {
					return err
				}
				for _, part := range content {
					if part.Type == "json" && reflect.DeepEqual(part.Value, expected) {
						resultReadable = true
					}
				}
			}
		}
		if !page.HasMore {
			break
		}
		after = page.NextAfter
	}
	if !resultReadable {
		return errors.New("authored result output was not readable through the public API while saving")
	}
	if sequence == 1 && g.clientReconnect {
		if err := writeNativeHandoffFile(filepath.Join(g.evidence, "client-held-"+turn.String()+".json"), map[string]any{
			"sessionId": session.String(), "turnId": turn.String(), "saveId": save,
		}); err != nil {
			return err
		}
	}
	err = g.pool.QueryRow(ctx, `SELECT p.id FROM computer_saves s JOIN sessions p ON (p.environment_id,p.computer_id)=(s.environment_id,s.computer_id) JOIN agents a ON(a.environment_id,a.id)=(p.environment_id,p.agent_id) JOIN turns t ON(t.environment_id,t.session_id)=(p.environment_id,p.id) WHERE s.environment_id=$1 AND s.id=$2 AND a.name='peer' AND t.status='running'`, g.environment, save).Scan(&peer)
	if err != nil {
		return err
	}
	count := func() (int64, error) {
		var n int64
		err := g.pool.QueryRow(ctx, `SELECT COALESCE(max(((convert_from(e.data,'UTF8')::jsonb->0->>'text')::jsonb->>'count')::bigint),0) FROM session_events e JOIN turns t ON(t.environment_id,t.id)=(e.environment_id,e.turn_id) WHERE e.environment_id=$1 AND e.session_id=$2 AND e.kind='turn.output' AND t.status='running'`, g.environment, peer).Scan(&n)
		return n, err
	}
	stillFinalizing := func() error {
		var valid bool
		err := g.pool.QueryRow(ctx, `SELECT t.status='finalizing' AND t.completion_save_id IS NULL AND s.status='captured' AND s.root_id IS NULL
      FROM computer_saves s JOIN turns t ON (t.environment_id,t.id)=(s.environment_id,s.turn_id)
      WHERE s.environment_id=$1 AND s.id=$2`, g.environment, save).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return errors.New("native Turn settled while publication was held")
		}
		return nil
	}
	before, err := count()
	if err != nil {
		return err
	}
	previous, advances := before, 0
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if sequence == 1 {
				var status string
				err := g.pool.QueryRow(ctx, `SELECT status FROM turns WHERE environment_id=$1 AND session_id=$2 AND seq=2`, g.environment, session).Scan(&status)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if err == nil && status != "queued" {
					return errors.New("follow-up did not remain queued during predecessor saving")
				}
				observation.QueuedFollowup = err == nil
			}
			if err := stillFinalizing(); err != nil {
				return err
			}
			after, err := count()
			if err != nil {
				return err
			}
			if after < previous {
				return errors.New("peer counter regressed during publication hold")
			}
			if after > previous {
				advances++
				previous = after
			}
			clientReady := sequence != 1 || !g.clientReconnect
			if sequence == 1 && g.clientReconnect {
				raw, err := os.ReadFile(filepath.Join(g.evidence, "client-resumed-"+turn.String()+".json"))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if err == nil {
					var receipt struct {
						SessionID   string `json:"sessionId"`
						TurnID      string `json:"turnId"`
						SaveID      string `json:"saveId"`
						FollowingID string `json:"followingId"`
					}
					if err := json.Unmarshal(raw, &receipt); err != nil {
						return err
					}
					if receipt.SessionID != session.String() || receipt.TurnID != turn.String() || receipt.SaveID != save {
						return errors.New("client reconnect receipt changed publication identity")
					}
					if err := g.pool.QueryRow(ctx, `SELECT status='queued' AND started_at IS NULL FROM turns WHERE environment_id=$1 AND session_id=$2 AND seq=2 AND id=$3`, g.environment, session, receipt.FollowingID).Scan(&clientReady); err != nil {
						return err
					}
					if !clientReady {
						return errors.New("client follow-up started before publication release")
					}
				}
			}
			if advances >= 5 && clientReady && (sequence != 1 || observation.QueuedFollowup) {
				if err := stillFinalizing(); err != nil {
					return err
				}
				g.mu.Lock()
				defer g.mu.Unlock()
				if g.observed == nil {
					g.observed = map[string]nativePublicationObservation{}
				}
				observation.PeerCounterBefore, observation.PeerCounterAfter = before, after
				observation.PublicationEnteredAt = started.UTC()
				observation.PublicationHoldMs = float64(time.Since(started)) / float64(time.Millisecond)
				g.observed[save] = observation
				return nil
			}
		}
	}
}
