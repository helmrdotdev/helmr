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
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/nbd"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"
)

// This fixture deliberately loses a customer process. It qualifies publication
// disposition and retained physical custody, not transparent process recovery.
func TestWorkerAgentAuthorityLoss(t *testing.T) {
	testWorkerAuthorityLoss(t, false)
}

func TestWorkerAgentRetainedNetworkRecovery(t *testing.T) {
	if os.Getenv("HELMR_NATIVE_NETWORK_RECOVERY_PROOF") != "1" {
		t.Skip("requires explicitly selected retained-network fault qualification")
	}
	testWorkerAuthorityLoss(t, true)
}

func testWorkerAuthorityLoss(t *testing.T, networkRecovery bool) {
	t.Helper()
	if os.Getenv("HELMR_NATIVE_EXECUTION_PROOF") != "1" || os.Getenv("HELMR_NATIVE_AUTHORITY_LOSS_PROOF") != "1" {
		t.Skip("requires an authorized disposable KVM host and isolated storage")
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
	for _, path := range []string{input.Worker, input.Bundle, input.RuntimeDescriptor, input.Runtime, input.Evidence, input.WorkerCgroup} {
		if !filepath.IsAbs(path) {
			t.Fatal("fixture paths must be absolute")
		}
	}
	if input.RestartPublicationOwner || !strings.Contains(input.CASURI, "/_verification/native-execution/") || !strings.Contains(input.PlatformURI, "/_verification/native-execution/") {
		t.Fatal("Worker-loss fixture requires its own isolated storage and a live CP")
	}
	baseWork := filepath.Join(filepath.Dir(input.Evidence), "w")
	if input.WorkerEnv["WORKER_WORK_DIR"] != baseWork || input.WorkerEnv["JAILER_CHROOT_DIR"] != filepath.Join(baseWork, "jailer") || input.WorkerEnv["WORKER_HOST_SECRET_PATH"] != filepath.Join(baseWork, "host-secret.json") {
		t.Fatal("fixture must own the configured Worker roots")
	}
	if version := input.WorkerEnv["JAILER_CGROUP_VERSION"]; version != "" && version != "2" {
		t.Fatal("fixture requires cgroup v2")
	}
	if err := os.Mkdir(input.Evidence, 0700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		published bool
		fault     string
	}{{"unpublished", false, ""}, {"published", true, ""}}
	if networkRecovery {
		cases = []struct {
			name      string
			published bool
			fault     string
		}{
			{"root-policy", false, "root-policy"}, {"namespace", true, "namespace"}, {"replacement-root", false, "replacement-root"},
		}
	}
	for index, scenario := range cases {
		if !t.Run(scenario.name, func(t *testing.T) {
			config := input
			config.Evidence = filepath.Join(input.Evidence, strconv.Itoa(index))
			config.CASURI += "/loss-" + strconv.Itoa(index)
			config.WorkerEnv = make(map[string]string, len(input.WorkerEnv))
			for key, value := range input.WorkerEnv {
				config.WorkerEnv[key] = value
			}
			work := baseWork + strconv.Itoa(index)
			config.WorkerEnv["WORKER_WORK_DIR"] = work
			config.WorkerEnv["JAILER_CHROOT_DIR"] = filepath.Join(work, "jailer")
			config.WorkerEnv["WORKER_HOST_SECRET_PATH"] = filepath.Join(work, "host-secret.json")
			if networkRecovery {
				// Preserve a full probe envelope alongside one retained owner.
				config.WorkerEnv["WORKER_EXECUTION_SLOTS"] = "2"
				config.WorkerEnv["WORKER_CAPACITY_VCPUS"] = "4"
				config.WorkerEnv["WORKER_CAPACITY_MEMORY_MIB"] = "8192"
			}
			if len(filepath.Join(work, "jailer/firecracker", strings.Repeat("0", 36), "root/vsock.sock")) > 107 {
				t.Fatal("fixture Unix socket path is too long")
			}
			if _, err := os.Lstat(work); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fixture working root already exists or cannot be inspected")
			}
			if err := os.Mkdir(config.Evidence, 0700); err != nil {
				t.Fatal(err)
			}
			runWorkerAuthorityLoss(t, config, scenario.published, scenario.fault)
		}) {
			break
		}
	}
}

type lossPublication struct {
	Save          workerapi.AgentSave
	Body          []byte
	Authorization string
	Code          int
	Err           error
}

type authorityLossGate struct {
	pool                             *pgxpool.Pool
	environment                      uuid.UUID
	next                             http.Handler
	published                        bool
	mu                               sync.Mutex
	turn                             string
	taken                            bool
	reached                          chan lossPublication
	release                          chan struct{}
	recovery, activation, completion atomic.Int64
}

func (g *authorityLossGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/worker/v1/instance/recover":
		g.recovery.Add(1)
	case "/worker/v1/instance/activate":
		g.activation.Add(1)
	case "/worker/v1/instance/drain/complete":
		g.completion.Add(1)
	}
	if r.URL.Path != "/worker/v1/computer-saves/publish" {
		g.next.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read publication", 500)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var publication workerapi.AgentSavePublication
	if err := json.Unmarshal(body, &publication); err != nil {
		http.Error(w, "decode publication", 500)
		return
	}
	g.mu.Lock()
	turn := g.turn
	g.mu.Unlock()
	var target bool
	if turn != "" {
		err = g.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND id=$2 AND turn_id=$3)`, g.environment, publication.Save.SaveID, turn).Scan(&target)
	}
	if err != nil {
		http.Error(w, "inspect publication", 500)
		return
	}
	if !target {
		g.next.ServeHTTP(w, r)
		return
	}
	g.mu.Lock()
	first := !g.taken
	g.taken = true
	g.mu.Unlock()
	observation := lossPublication{Save: publication.Save, Body: body, Authorization: r.Header.Get("Authorization")}
	if first && g.published {
		response := httptest.NewRecorder()
		g.next.ServeHTTP(response, r)
		observation.Code = response.Code
		if response.Code != http.StatusNoContent {
			observation.Err = fmt.Errorf("publication before ACK loss returned %d", response.Code)
		}
	}
	if first {
		g.reached <- observation
	}
	select {
	case <-g.release:
	case <-r.Context().Done():
	}
	// Neither the unpublished request nor the held committed reply is delivered.
	// Normal fault flow opens this after owner death; failure teardown may
	// open it earlier so shutdown is not blocked by a fixture-held request.
	http.Error(w, "publication connection lost", http.StatusServiceUnavailable)
}

type nativeWorkerProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startLossWorker(input nativeExecutionConfig, environment map[string]string, sequence int) (*nativeWorkerProcess, error) {
	return startNativeWorker(input, environment, fmt.Sprintf("worker-%d.log", sequence))
}

func startNativeWorker(input nativeExecutionConfig, environment map[string]string, logName string) (*nativeWorkerProcess, error) {
	log, err := os.OpenFile(filepath.Join(input.Evidence, logName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	cgroup, err := os.Open(input.WorkerCgroup)
	if err != nil {
		log.Close()
		return nil, err
	}
	defer cgroup.Close()
	process := &nativeWorkerProcess{cmd: exec.Command(input.Worker), done: make(chan struct{})}
	process.cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgroup.Fd())}
	process.cmd.Env = nativeExecutionEnv(environment)
	process.cmd.Stdout, process.cmd.Stderr = log, log
	if err := process.cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	go func() { process.err = errors.Join(process.cmd.Wait(), log.Close()); close(process.done) }()
	return process, nil
}

func (p *nativeWorkerProcess) wait(ctx context.Context) error {
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func awaitLossCondition(ctx context.Context, condition func() (bool, error)) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		ok, err := condition()
		if err != nil || ok {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Keep the authoritative database and protected CP restart input until the
// operator has also qualified the outer physical audit. Test teardown must not
// erase reconciliation authority when a helper or consumer remains unresolved.
func openLossDatabase(t *testing.T, input nativeExecutionConfig, key []byte) dbtest.Database {
	t.Helper()
	config, err := pgxpool.ParseConfig(os.Getenv("HELMR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(t.Context(), config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "helmr_loss_" + strings.ReplaceAll(uuid.NewV7().String(), "-", "_")
	parsed, err := url.Parse(os.Getenv("HELMR_TEST_DATABASE_URL"))
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("authority-loss fixture requires a PostgreSQL URL")
	}
	parsed.Path = "/" + name
	query := parsed.Query()
	query.Del("dbname")
	parsed.RawQuery = query.Encode()
	dsn := parsed.String()
	config, err = pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Database != name {
		t.Fatal("PostgreSQL URL overrides the isolated database")
	}
	raw, err := json.Marshal(nativeControlPlaneInput{DSN: dsn, Key: key, Config: input})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(input.Evidence, "recovery-private.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(raw)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	pool, err := dbpool.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return dbtest.Database{DSN: dsn, Pool: pool}
}

func runWorkerAuthorityLoss(t *testing.T, input nativeExecutionConfig, published bool, networkFault string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	proof := map[string]any{"publishedBeforeLoss": published, "networkFault": networkFault}
	defer func() {
		raw, err := json.MarshalIndent(proof, "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(input.Evidence, "authority-loss.json"), raw, 0600)
		}
		if err != nil {
			t.Error("retain loss evidence", err)
		}
	}()
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	database := openLossDatabase(t, input, keyBytes)
	if err := schema.Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	env, key, enrollment := seedNativeExecution(t, database.Pool)
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
	gate := &authorityLossGate{pool: database.Pool, environment: env, next: cp.proxy, published: published, reached: make(chan lossPublication, 1), release: make(chan struct{})}
	server := httptest.NewServer(gate)
	defer server.Close()
	releaseGate := sync.OnceFunc(func() { close(gate.release) })
	defer releaseGate()
	apiClient, err := client.New(server.URL, client.WithBearerToken(key))
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(input.Evidence, "enrollment-token")
	if err := os.WriteFile(tokenPath, []byte(enrollment), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tokenPath)
	workerEnv := make(map[string]string, len(input.WorkerEnv)+8)
	for key, value := range input.WorkerEnv {
		workerEnv[key] = value
	}
	workerEnv["CONTROL_PLANE_URL"], workerEnv["CAS_URI"], workerEnv["PLATFORM_STORE_URI"] = server.URL, input.CASURI, input.PlatformURI
	workerEnv["WORKER_ENROLLMENT_TOKEN_FILE"], workerEnv["WORKER_POOL_NAME"] = tokenPath, "native-proof"
	workerEnv["WORKER_RESOURCE_ID"] = "loss-proof-" + uuid.NewV7().String()
	workerEnv["CHECKPOINT_ENCRYPTION_KEY"] = base64.StdEncoding.EncodeToString(keyBytes)
	active, err := startLossWorker(input, workerEnv, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		releaseGate()
		select {
		case <-active.done:
			return
		default:
		}
		cleanup, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		if err := retireNativeExecution(cleanup, database.Pool, apiClient, env); err != nil {
			t.Error("fixture retirement requires operator audit", err)
		}
		shutdown, stopShutdown := context.WithTimeout(context.Background(), 40*time.Second)
		defer stopShutdown()
		_ = active.cmd.Process.Signal(syscall.SIGTERM)
		if err := active.wait(shutdown); err != nil {
			t.Error("worker shutdown requires operator audit", err)
			_ = active.cmd.Process.Kill()
			<-active.done
			return
		}
		proof["workerStoppedCleanly"] = true
	}()
	closed, err := bundle.ReadDirectory(input.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := apiClient.PlanDeploymentBundleUploads(ctx, closed.BundleJSON, client.EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, upload := range plan.Uploads {
		path, ok := closed.Objects[upload.Digest]
		if !ok {
			t.Fatal("upload escaped closed bundle")
		}
		if err := apiClient.UploadDeploymentBundleObject(ctx, upload, path, nil); err != nil {
			t.Fatal(err)
		}
	}
	deployment, err := apiClient.FinalizeDeploymentBundle(ctx, api.FinalizeDeploymentBundleRequest{IdempotencyKey: "loss-proof", BundleDigest: plan.BundleDigest}, client.EnvironmentScopeOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apiClient.PromoteDeployment(ctx, deployment.ID, client.EnvironmentScopeOptions{}); err != nil {
		t.Fatal(err)
	}
	started, err := apiClient.StartAgent(ctx, "peer", api.StartAgentRequest{Input: json.RawMessage(`[{"type":"text","text":"hold"}]`), IdempotencyKey: "loss-original"}, client.EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	proof["sessionId"], proof["turnId"] = started.SessionID, started.TurnID
	gate.mu.Lock()
	gate.turn = started.TurnID
	gate.mu.Unlock()
	var output api.AgentSessionEvent
	err = awaitLossCondition(ctx, func() (bool, error) {
		select {
		case <-active.done:
			return false, fmt.Errorf("Worker exited before authored output: %v", active.err)
		default:
		}
		page, err := apiClient.ReadSessionEvents(ctx, started.SessionID, client.SessionEventReadOptions{Limit: 100})
		if err != nil {
			return false, err
		}
		for _, event := range page.Records {
			if event.TurnID != nil && *event.TurnID == started.TurnID && event.Kind == "turn.output" {
				output = event
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatal("authored output before loss", err)
	}
	proof["publicOutputBeforeLoss"] = output
	if _, err := apiClient.SendSessionMessage(ctx, started.SessionID, started.TurnID, api.SessionDataRequest{Data: json.RawMessage(`"release"`), IdempotencyKey: "loss-release"}, client.EnvironmentScopeOptions{}); err != nil {
		t.Fatal(err)
	}
	var publication lossPublication
	select {
	case publication = <-gate.reached:
	case <-active.done:
		t.Fatalf("Worker exited before publication: %v", active.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if publication.Err != nil {
		t.Fatal(publication.Err)
	}
	proof["saveId"] = publication.Save.SaveID
	custody, err := inspectLossCustody(ctx, database.Pool, env, publication.Save, input, active.cmd.Process.Pid)
	if err != nil {
		t.Fatal("establish live fixture custody", err)
	}
	defer custody.close()
	proof["custody"] = custody
	before, err := lossDatabaseState(ctx, database.Pool, env, started, publication.Save.SaveID)
	if err != nil {
		t.Fatal(err)
	}
	var beforeFields map[string]json.RawMessage
	if err := json.Unmarshal(before, &beforeFields); err != nil {
		t.Fatal(err)
	}
	if string(beforeFields["processCount"]) != "1" {
		t.Fatal("fixture did not establish exactly one original process")
	}
	proof["beforeLoss"] = before
	if err := active.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	var killed *exec.ExitError
	if err := active.wait(ctx); !errors.As(err, &killed) || killed.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("Worker-only SIGKILL not observed: %v", err)
	}
	proof["workerKilledAt"] = time.Now().UTC()
	var retainedNetwork *lossNetworkFault
	if networkFault != "" {
		retainedNetwork = installLossNetworkFault(t, input, workerEnv, custody.Instance, networkFault)
	}
	err = awaitLossCondition(ctx, func() (bool, error) {
		raw, err := os.ReadFile(filepath.Join(custody.Arena, "claim.json"))
		if err != nil {
			return false, err
		}
		var claim struct{ Phase string }
		if err := json.Unmarshal(raw, &claim); err != nil {
			return false, err
		}
		return claim.Phase == "quarantined-control-lost", nil
	})
	if err != nil {
		t.Fatal("helper did not record control loss", err)
	}
	if exited, err := custody.Helper.exited(); err != nil || exited {
		t.Fatalf("known helper did not survive Worker-only loss: %t %v", exited, err)
	}
	if err := nbd.VerifyIdleDevices([]string{custody.Device}); !errors.Is(err, unix.EBUSY) {
		t.Fatalf("retained helper did not hold exclusive custody: %v", err)
	}
	retained, err := lossArenaFiles(custody.Arena, custody.Device)
	if err != nil || len(retained) == 0 {
		t.Fatalf("retained cut evidence: %d %v", len(retained), err)
	}
	proof["retainedFilesAfterLoss"] = retained
	counts := []int64{gate.recovery.Load(), gate.activation.Load(), gate.completion.Load()}
	restarted, err := startLossWorker(input, workerEnv, 1)
	if err != nil {
		t.Fatal(err)
	}
	active = restarted
	if err := active.wait(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("Worker did not exit on retained custody: %v", err)
	}
	blockedLog, err := os.ReadFile(filepath.Join(input.Evidence, "worker-1.log"))
	if err != nil || !bytes.Contains(blockedLog, []byte("computer arena "+custody.Arena+" requires attachment reconciliation")) {
		t.Fatalf("Worker did not reject attachment custody: %v", err)
	}
	if !reflect.DeepEqual(counts, []int64{gate.recovery.Load(), gate.activation.Load(), gate.completion.Load()}) {
		t.Fatal("blocked Worker reported recovery, activated or completed drain")
	}
	after, err := lossArenaFiles(custody.Arena, custody.Device)
	if err != nil || !reflect.DeepEqual(retained, after) {
		t.Fatalf("blocked restart changed retained dependencies: %v", err)
	}
	wantSave, wantTurn := "failed", "interrupted"
	if published {
		wantSave, wantTurn = "published", "completed"
	}
	err = awaitLossCondition(ctx, func() (bool, error) {
		var settled bool
		err := database.Pool.QueryRow(ctx, `SELECT s.status=$3 AND t.status=$4 AND l.status='lost' AND l.fenced_at IS NULL
 AND l.reserved_cpu_millis>0 AND l.reserved_memory_bytes>0 AND l.reserved_scratch_bytes>0 AND h.current_epoch>l.worker_epoch
 FROM computer_saves s JOIN turns t ON (t.environment_id,t.id)=(s.environment_id,s.turn_id)
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(s.environment_id,s.computer_id,s.computer_lease_epoch)
 JOIN worker_hosts h ON h.id=l.worker_host_id WHERE s.environment_id=$1 AND s.id=$2`, env, publication.Save.SaveID, wantSave, wantTurn).Scan(&settled)
		return settled, err
	})
	if err != nil {
		t.Fatal("independent lost-save disposition with retained physical charge", err)
	}
	lost, err := lossDatabaseState(ctx, database.Pool, env, started, publication.Save.SaveID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sameLossSaveEvidence(before, lost); err != nil {
		t.Fatal(err)
	}
	proof["lostBeforePhysicalRelease"] = lost
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/worker/v1/computer-saves/publish", bytes.NewReader(publication.Body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", publication.Authorization)
	request.Header.Set("Content-Type", "application/json")
	// Bypass only the fault gate to exercise the real CP rejection after epoch loss.
	stale := httptest.NewRecorder()
	cp.proxy.ServeHTTP(stale, request)
	if stale.Code != http.StatusUnauthorized && stale.Code != http.StatusForbidden && stale.Code != http.StatusConflict {
		t.Fatalf("stale publication returned %d", stale.Code)
	}
	proof["stalePublicationStatus"] = stale.Code
	for _, consumer := range custody.Consumers {
		if exited, err := consumer.exited(); err != nil || !exited {
			t.Fatalf("consumer cessation is unproved: pid=%d exited=%t err=%v", consumer.PID, exited, err)
		}
	}
	if retainedNetwork == nil {
		if _, err := os.Lstat(custody.Cgroup); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("consumer cgroup absence is unproved: %v", err)
		}
	} else {
		retainedNetwork.assertRetained(t, blockedLog)
		// Network cleanup failed before cgroup removal. Process exit above is
		// proved independently; custody must remain until network closure.
		if _, err := os.Stat(custody.Cgroup); err != nil {
			t.Fatalf("lost retained cgroup: %v", err)
		}
	}
	proof["consumerAbsenceObservedAt"] = time.Now().UTC()
	// This explicit fixture operation uses a pidfd retained from the known live
	// helper before the fault, after consumer absence and Save disposition.
	if err := unix.PidfdSendSignal(custody.Helper.fd, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal("release exact known helper", err)
	}
	if err := awaitLossCondition(ctx, custody.Helper.exited); err != nil {
		t.Fatal("known helper exit", err)
	}
	if err := awaitLossCondition(ctx, func() (bool, error) { return nbd.VerifyIdleDevices([]string{custody.Device}) == nil, nil }); err != nil {
		t.Fatal("device did not become idle after exact release", err)
	}
	proof["helperReleaseObservedAt"] = time.Now().UTC()
	if err := os.RemoveAll(custody.Arena); err != nil {
		t.Fatal(err)
	}
	nextSequence := 2
	if retainedNetwork != nil {
		restarted, err = startLossWorker(input, workerEnv, nextSequence)
		if err != nil {
			t.Fatal(err)
		}
		active = restarted
		if err := active.wait(ctx); err == nil || ctx.Err() != nil {
			t.Fatalf("retained network admitted Worker: %v", err)
		}
		blockedLog, err := os.ReadFile(filepath.Join(input.Evidence, "worker-2.log"))
		if err != nil {
			t.Fatal(err)
		}
		retainedNetwork.assertRetained(t, blockedLog)
		afterCounts := []int64{gate.recovery.Load(), gate.activation.Load(), gate.completion.Load()}
		if networkFault != "namespace" {
			if !bytes.Contains(blockedLog, []byte("overlaps live")) || !reflect.DeepEqual(counts, afterCounts) {
				t.Fatalf("root residue bypassed preflight: %v -> %v", counts, afterCounts)
			}
		} else if !bytes.Contains(blockedLog, []byte("activate worker:")) || afterCounts[0] != counts[0]+1 || afterCounts[1] != counts[1]+1 || afterCounts[2] != counts[2] {
			t.Fatalf("namespace residue did not reach fenced activation: %v -> %v", counts, afterCounts)
		}
		var unfenced bool
		if err := database.Pool.QueryRow(ctx, `SELECT fenced_at IS NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, custody.Computer, custody.Epoch).Scan(&unfenced); err != nil || !unfenced {
			t.Fatalf("quarantine fenced allocation: %v %v", unfenced, err)
		}
		proof["networkBlockedStartup"] = map[string]any{"countsBefore": counts, "countsAfter": afterCounts, "oldAllocationUnfenced": unfenced, "observedAt": time.Now().UTC()}
		retainedNetwork.clear(t)
		nextSequence++
	}
	restarted, err = startLossWorker(input, workerEnv, nextSequence)
	if err != nil {
		t.Fatal(err)
	}
	active = restarted
	err = awaitLossCondition(ctx, func() (bool, error) {
		select {
		case <-active.done:
			return false, fmt.Errorf("Worker exited before reactivation: %v", active.err)
		default:
		}
		var ready bool
		err := database.Pool.QueryRow(ctx, `SELECT h.status='active' AND h.current_epoch>l.worker_epoch AND l.fenced_at IS NOT NULL
 FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3`, env, custody.Computer, custody.Epoch).Scan(&ready)
		return ready, err
	})
	if err != nil {
		t.Fatal("ordinary reactivation after explicit reconciliation", err)
	}
	var unchanged bool
	err = database.Pool.QueryRow(ctx, `SELECT s.status=$3 AND t.status=$4
 AND (SELECT count(*) FROM session_processes WHERE environment_id=$1 AND session_id=t.session_id)=1
 AND NOT EXISTS(SELECT 1 FROM session_processes WHERE environment_id=$1 AND session_id=t.session_id AND status IN ('starting','ready'))
 AND (SELECT count(*) FROM session_events WHERE environment_id=$1 AND turn_id=t.id AND kind='turn.'||$4)=1
 FROM computer_saves s JOIN turns t ON (t.environment_id,t.id)=(s.environment_id,s.turn_id)
 WHERE s.environment_id=$1 AND s.id=$2`, env, publication.Save.SaveID, wantSave, wantTurn).Scan(&unchanged)
	if err != nil || !unchanged {
		t.Fatalf("lost process was replayed or its outcome changed: %t %v", unchanged, err)
	}
	publicTurn, err := apiClient.RetrieveSessionTurn(ctx, started.SessionID, started.TurnID, client.EnvironmentScopeOptions{})
	if err != nil || publicTurn.Status != wantTurn {
		t.Fatalf("public loss outcome differs: %s %v", publicTurn.Status, err)
	}
	reconciled, err := lossDatabaseState(ctx, database.Pool, env, started, publication.Save.SaveID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sameLossSaveEvidence(before, reconciled); err != nil {
		t.Fatal(err)
	}
	proof["afterPhysicalReconciliation"] = reconciled
	page, err := apiClient.ReadSessionEvents(ctx, started.SessionID, client.SessionEventReadOptions{After: output.Sequence - 1, Limit: 100})
	if err != nil || len(page.Records) == 0 || page.Records[0].Sequence != output.Sequence || !bytes.Equal(page.Records[0].Data, output.Data) {
		t.Fatalf("public output was not retained: %v", err)
	}
	if err := retireNativeExecution(ctx, database.Pool, apiClient, env); err != nil {
		t.Fatal("retire lost Computer", err)
	}
	reuse, err := apiClient.StartAgent(ctx, "peer", api.StartAgentRequest{Input: json.RawMessage(`[{"type":"text","text":"reuse"}]`), IdempotencyKey: "loss-reuse"}, client.EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = awaitLossCondition(ctx, func() (bool, error) {
		select {
		case <-active.done:
			return false, fmt.Errorf("Worker exited before fresh completion: %v", active.err)
		default:
		}
		var complete bool
		err := database.Pool.QueryRow(ctx, `SELECT t.status='completed' AND s.status='published' AND s.root_id IS NOT NULL
 FROM turns t LEFT JOIN computer_saves s ON (s.environment_id,s.id)=(t.environment_id,t.completion_save_id)
 WHERE t.environment_id=$1 AND t.id=$2`, env, reuse.TurnID).Scan(&complete)
		return complete, err
	})
	if err != nil {
		t.Fatal("fresh Computer did not complete after device reuse", err)
	}
	var reuseSave workerapi.AgentSave
	reuseSave.EnvironmentID = env.String()
	err = database.Pool.QueryRow(ctx, `SELECT s.id::text,s.computer_lease_epoch FROM turns t JOIN computer_saves s ON (s.environment_id,s.id)=(t.environment_id,t.completion_save_id) WHERE t.environment_id=$1 AND t.id=$2`, env, reuse.TurnID).Scan(&reuseSave.SaveID, &reuseSave.LeaseEpoch)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := inspectLossCustody(ctx, database.Pool, env, reuseSave, input, active.cmd.Process.Pid)
	if err != nil {
		t.Fatal("observe fresh device ownership", err)
	}
	reused.close()
	if reused.Device != custody.Device || reused.Instance == custody.Instance || (reused.Helper.PID == custody.Helper.PID && reused.Helper.Start == custody.Helper.Start) {
		t.Fatal("fresh Computer did not establish new ownership of the released device")
	}
	if retainedNetwork != nil {
		raw, err := os.ReadFile(filepath.Join(workerEnv["WORKER_WORK_DIR"], "vms", "guest", reused.Instance, "network.json"))
		if err != nil {
			t.Fatal(err)
		}
		var network struct {
			AllocationIndex *uint32 `json:"allocation_index"`
		}
		if err := json.Unmarshal(raw, &network); err != nil || network.AllocationIndex == nil || *network.AllocationIndex != retainedNetwork.allocationIndex {
			t.Fatalf("fresh Computer did not reuse recovered address index: %v %v", network.AllocationIndex, err)
		}
		proof["reusedNetworkIndex"] = *network.AllocationIndex
	}
	proof["reusedDevice"] = reused.Device
	proof["reuseTurnId"] = reuse.TurnID
	if published {
		if err := retireNativeExecution(ctx, database.Pool, apiClient, env); err != nil {
			t.Fatal(err)
		}
		// This marker is created only after real allocations have retired. It has
		// no device or Save owner and belongs solely to this refusal test.
		arena := filepath.Join(workerEnv["WORKER_WORK_DIR"], "tmp", "computer-drain-proof")
		if err := os.Mkdir(arena, 0700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(arena, "config.json")
		markerBytes := []byte("fixture-owned drain refusal marker")
		if err := os.WriteFile(marker, markerBytes, 0600); err != nil {
			t.Fatal(err)
		}
		completed := gate.completion.Load()
		drain := exec.CommandContext(ctx, input.Worker, "drain", "--wait=false")
		drain.Env = nativeExecutionEnv(workerEnv)
		diagnostic, err := drain.CombinedOutput()
		if writeErr := os.WriteFile(filepath.Join(input.Evidence, "drain-command.log"), diagnostic, 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
		if err != nil {
			t.Fatal("ordinary drain command", err)
		}
		if err := active.wait(ctx); err == nil || ctx.Err() != nil {
			t.Fatalf("drain did not exit on retained custody: %v", err)
		}
		drainLog, err := os.ReadFile(filepath.Join(input.Evidence, fmt.Sprintf("worker-%d.log", nextSequence)))
		refusal := "computer arena " + arena + " requires attachment reconciliation: retained config.json"
		if err != nil || !bytes.Contains(drainLog, []byte(refusal)) {
			t.Fatalf("drain did not reject the fixture attachment marker: %v", err)
		}
		if gate.completion.Load() != completed {
			t.Fatal("drain completed despite attachment residue")
		}
		if _, err := os.Lstat(filepath.Join(workerEnv["WORKER_WORK_DIR"], "drain-complete")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("false drain completion marker: %v", err)
		}
		kept, err := os.ReadFile(marker)
		if err != nil || !bytes.Equal(kept, markerBytes) {
			t.Fatalf("drain refusal changed evidence: %v", err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(arena); err != nil {
			t.Fatal(err)
		}
		restarted, err = startLossWorker(input, workerEnv, nextSequence+1)
		if err != nil {
			t.Fatal(err)
		}
		active = restarted
		if err := active.wait(ctx); err != nil {
			t.Fatal("clean ordinary drain did not finish", err)
		}
		if gate.completion.Load() != completed+1 {
			t.Fatal("clean drain completion was not observed exactly once")
		}
		if _, err := os.Stat(filepath.Join(workerEnv["WORKER_WORK_DIR"], "drain-complete")); err != nil {
			t.Fatal("clean drain receipt missing", err)
		}
		proof["drainRefusalAndRetry"] = true
		proof["workerStoppedCleanly"] = true
	}
	proof["recoveryCalls"], proof["activationCalls"] = gate.recovery.Load(), gate.activation.Load()
	proof["completedAt"] = time.Now().UTC()
}

type lossOwnedProcess struct {
	PID   int
	Start string
	Boot  string
	Root  string `json:",omitempty"`
	Arg0  string `json:",omitempty"`
	fd    int
}

func retainLossProcess(pid int) (lossOwnedProcess, error) {
	p := lossOwnedProcess{PID: pid, fd: -1}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return p, err
	}
	p.fd = fd
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err == nil {
		p.Boot = strings.TrimSpace(string(boot))
		p.Start, _, err = lossProcessStat(pid)
	}
	if err == nil {
		var exited bool
		exited, err = p.exited()
		if exited {
			err = errors.New("process exited while live ownership was inspected")
		}
	}
	if err != nil {
		unix.Close(fd)
		p.fd = -1
	}
	return p, err
}

func lossProcessStat(pid int) (start string, parent int, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, err
	}
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return "", 0, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return "", 0, errors.New("incomplete process stat")
	}
	parent, err = strconv.Atoi(fields[1])
	return fields[19], parent, err
}

func (p lossOwnedProcess) exited() (bool, error) {
	polls := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(polls, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		break
	}
	if polls[0].Revents&(unix.POLLNVAL|unix.POLLERR) != 0 {
		return false, errors.New("retained process handle is invalid")
	}
	return polls[0].Revents&unix.POLLIN != 0, nil
}

type lossCustody struct {
	Arena, Device, Cgroup, Computer, Instance, Host string
	Epoch, WorkerEpoch                              int64
	Helper                                          lossOwnedProcess
	Consumers                                       []lossOwnedProcess
}

func (c *lossCustody) close() {
	if c.Helper.fd >= 0 {
		_ = unix.Close(c.Helper.fd)
		c.Helper.fd = -1
	}
	for i := range c.Consumers {
		_ = unix.Close(c.Consumers[i].fd)
		c.Consumers[i].fd = -1
	}
}

func inspectLossCustody(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, save workerapi.AgentSave, input nativeExecutionConfig, workerPID int) (_ *lossCustody, err error) {
	c := &lossCustody{Helper: lossOwnedProcess{fd: -1}}
	defer func() {
		if err != nil {
			c.close()
		}
	}()
	err = pool.QueryRow(ctx, `SELECT l.computer_id::text,l.computer_instance_id::text,l.worker_host_id::text,l.epoch,l.worker_epoch
 FROM computer_saves s JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(s.environment_id,s.computer_id,s.computer_lease_epoch)
 WHERE s.environment_id=$1 AND s.id=$2 AND l.status='active' AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp()`, env, save.SaveID).Scan(&c.Computer, &c.Instance, &c.Host, &c.Epoch, &c.WorkerEpoch)
	if err != nil {
		return nil, err
	}
	c.Arena = filepath.Join(input.WorkerEnv["WORKER_WORK_DIR"], "tmp", "computer-"+c.Instance+"-"+strconv.FormatInt(c.Epoch, 10))
	info, err := os.Lstat(c.Arena)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("expected private arena: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(c.Arena, "claim.json"))
	if err != nil {
		return nil, err
	}
	var claim struct {
		Device, Boot, ProcessStat string
		PID                       int
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return nil, err
	}
	allowed := false
	for _, device := range strings.Fields(input.WorkerEnv["WORKER_COMPUTER_DEVICES"]) {
		allowed = allowed || device == claim.Device
	}
	if !allowed {
		return nil, errors.New("claim device is outside the fixture allowlist")
	}
	c.Device = claim.Device
	c.Helper, err = retainLossProcess(claim.PID)
	if err != nil {
		return nil, err
	}
	start, parent, err := lossProcessStat(claim.PID)
	if err != nil || parent != workerPID || start != c.Helper.Start || c.Helper.Boot != claim.Boot {
		return nil, fmt.Errorf("helper live parent/identity mismatch: %v", err)
	}
	end := strings.LastIndexByte(claim.ProcessStat, ')')
	if end < 0 {
		return nil, errors.New("invalid claim process identity")
	}
	fields := strings.Fields(claim.ProcessStat[end+1:])
	if len(fields) < 20 || fields[19] != c.Helper.Start {
		return nil, errors.New("claim does not match the live helper start")
	}
	args, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", claim.PID))
	if err != nil || !bytes.Equal(args, []byte(input.Worker+"\x00--nbd-helper\x00"+c.Arena+"\x00")) {
		return nil, fmt.Errorf("helper is not the exact fixture child: %v", err)
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", claim.PID))
	if err != nil || exe != input.Worker {
		return nil, fmt.Errorf("helper executable changed: %v", err)
	}
	c.Cgroup = filepath.Join("/sys/fs/cgroup/firecracker", c.Instance)
	pids, err := os.ReadFile(filepath.Join(c.Cgroup, "cgroup.procs"))
	if err != nil || len(strings.Fields(string(pids))) == 0 {
		return nil, fmt.Errorf("no live consumer in the exact cgroup: %v", err)
	}
	for _, text := range strings.Fields(string(pids)) {
		pid, err := strconv.Atoi(text)
		if err != nil {
			return nil, err
		}
		consumer, err := retainLossProcess(pid)
		if err != nil {
			return nil, err
		}
		c.Consumers = append(c.Consumers, consumer)
		procRoot := fmt.Sprintf("/proc/%d/root", pid)
		root, err := os.Readlink(procRoot)
		if err != nil {
			return nil, err
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return nil, err
		}
		c.Consumers[len(c.Consumers)-1].Root = root
		c.Consumers[len(c.Consumers)-1].Arg0 = strings.SplitN(string(cmdline), "\x00", 2)[0]
		expectedRoot := filepath.Join(input.WorkerEnv["JAILER_CHROOT_DIR"], "firecracker", c.Instance, "root")
		// procfs renders the target through the caller's mount namespace. Bind
		// mounts can give the same directory a different textual path. Require
		// the exact directory identity, not the symlink's path spelling.
		expected, err := os.Lstat(expectedRoot)
		if err != nil || !expected.IsDir() {
			return nil, fmt.Errorf("consumer expected root %q is not a directory: %v", expectedRoot, err)
		}
		actual, err := os.Stat(procRoot)
		if err != nil || !os.SameFile(expected, actual) {
			return nil, fmt.Errorf("consumer %d command %q root %q does not match live fixture directory %q: %v", pid, c.Consumers[len(c.Consumers)-1].Arg0, root, expectedRoot, err)
		}
	}
	return c, nil
}

func lossArenaFiles(root, device string) (map[string]string, error) {
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "claim.json" || rel == "claim.json.tmp" || rel == "nbd.sock" {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected symlink in retained arena")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if rel == filepath.Join("jail", "computer.nbd") {
			// The attachment helper exposes this block node into the jail.
			// Preserve its identity without opening or reading the live device.
			declared, err := os.Lstat(device)
			if err != nil {
				return fmt.Errorf("inspect declared attachment device: %w", err)
			}
			if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 ||
				declared.Mode()&os.ModeDevice == 0 || declared.Mode()&os.ModeCharDevice != 0 {
				return errors.New("retained attachment node is not the declared block device")
			}
			node, expected := info.Sys().(*syscall.Stat_t), declared.Sys().(*syscall.Stat_t)
			if node.Rdev != expected.Rdev {
				return errors.New("retained attachment node names another device")
			}
			files[rel] = fmt.Sprintf("block:%d:%d:%d:%o:%d:%d", node.Dev, node.Ino, node.Rdev, node.Mode, node.Uid, node.Gid)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected non-file dependency %s (%s) in retained arena", rel, info.Mode())
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		count, copyErr := io.Copy(hash, file)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return err
		}
		files[rel] = fmt.Sprintf("%d:%x", count, hash.Sum(nil))
		return nil
	})
	return files, err
}

func lossDatabaseState(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, turn api.StartAgentResponse, save string) (json.RawMessage, error) {
	var state json.RawMessage
	err := pool.QueryRow(ctx, `SELECT jsonb_build_object('saveId',s.id,'saveStatus',s.status,'rootId',s.root_id,
 'capturedRoot',encode(s.captured_root_digest,'hex'),'captureEvidence',s.capture_evidence,'publicationEvidence',s.publication_evidence,
 'turnId',t.id,'turnStatus',t.status,'result',t.result,'terminalAt',t.terminal_at,
 'leaseStatus',l.status,'fencedAt',l.fenced_at,'workerEpoch',l.worker_epoch,'hostEpoch',h.current_epoch,
 'cpuMillis',l.reserved_cpu_millis,'memoryBytes',l.reserved_memory_bytes,'scratchBytes',l.reserved_scratch_bytes,
 'processCount',(SELECT count(*) FROM session_processes WHERE environment_id=$1 AND session_id=$3))
 FROM computer_saves s JOIN turns t ON (t.environment_id,t.id)=(s.environment_id,s.turn_id)
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(s.environment_id,s.computer_id,s.computer_lease_epoch)
 JOIN worker_hosts h ON h.id=l.worker_host_id WHERE s.environment_id=$1 AND s.id=$2`, env, save, turn.SessionID).Scan(&state)
	return state, err
}

func sameLossSaveEvidence(before, after json.RawMessage) error {
	var original, observed map[string]json.RawMessage
	if err := json.Unmarshal(before, &original); err != nil {
		return err
	}
	if err := json.Unmarshal(after, &observed); err != nil {
		return err
	}
	for _, field := range []string{"saveId", "rootId", "capturedRoot", "captureEvidence", "publicationEvidence", "turnId", "result"} {
		if !bytes.Equal(original[field], observed[field]) {
			return fmt.Errorf("Worker loss changed retained %s", field)
		}
	}
	return nil
}

func TestLossProcessHandleTracksExactExit(t *testing.T) {
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	owned, err := retainLossProcess(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(owned.fd)
	if gone, err := owned.exited(); err != nil || gone {
		t.Fatalf("live child incorrectly absent: %t %v", gone, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if gone, err := owned.exited(); err != nil || !gone {
		t.Fatalf("joined child not absent: %t %v", gone, err)
	}
}

func TestLossArenaInventoryRejectsAmbiguousDependencies(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "version")
			switch kind {
			case "regular":
				if err := os.WriteFile(path, []byte("retained"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			files, err := lossArenaFiles(root, "")
			if kind != "regular" {
				if err == nil {
					t.Fatal("ambiguous dependency accepted")
				}
				return
			}
			if err != nil || len(files) != 1 {
				t.Fatalf("retained inventory: %v %v", files, err)
			}
			if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			next, err := lossArenaFiles(root, "")
			if err != nil || reflect.DeepEqual(files, next) {
				t.Fatal("dependency mutation was not detected", err)
			}
		})
	}
}

func TestLossArenaInventoryPreservesDeclaredBlockNode(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_DEVICE_NODE_TEST") != "1" {
		t.Skip("requires disposable Linux filesystem with device-node permission")
	}
	for _, kind := range []string{"declared", "different-device", "other-path", "regular", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			declared := filepath.Join(t.TempDir(), "declared")
			rdev := int(unix.Mkdev(43, 0))
			if err := unix.Mknod(declared, unix.S_IFBLK|0600, rdev); err != nil {
				t.Fatal(err)
			}
			jail := filepath.Join(root, "jail")
			if err := os.Mkdir(jail, 0700); err != nil {
				t.Fatal(err)
			}
			node := filepath.Join(jail, "computer.nbd")
			switch kind {
			case "regular":
				if err := os.WriteFile(node, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(declared, node); err != nil {
					t.Fatal(err)
				}
			default:
				if kind == "different-device" {
					rdev = int(unix.Mkdev(43, 1))
				}
				if kind == "other-path" {
					node = filepath.Join(jail, "other.nbd")
				}
				if err := unix.Mknod(node, unix.S_IFBLK|0600, rdev); err != nil {
					t.Fatal(err)
				}
			}
			before, err := lossArenaFiles(root, declared)
			if kind != "declared" {
				if err == nil {
					t.Fatal("unexpected device dependency accepted")
				}
				return
			}
			if err != nil || len(before) != 1 {
				t.Fatalf("declared device inventory: %v %v", before, err)
			}
			if err := os.Chmod(node, 0640); err != nil {
				t.Fatal(err)
			}
			after, err := lossArenaFiles(root, declared)
			if err != nil || reflect.DeepEqual(before, after) {
				t.Fatal("device metadata change not detected", err)
			}
		})
	}
}

func TestLossSaveEvidencePreservesIdentity(t *testing.T) {
	before := json.RawMessage(`{"saveId":"save","rootId":null,"capturedRoot":"cut","captureEvidence":"capture","publicationEvidence":null,"turnId":"turn","result":{"value":1},"saveStatus":"captured"}`)
	var after map[string]json.RawMessage
	if err := json.Unmarshal(before, &after); err != nil {
		t.Fatal(err)
	}
	after["saveStatus"] = json.RawMessage(`"failed"`)
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if err := sameLossSaveEvidence(before, raw); err != nil {
		t.Fatal(err)
	}
	after["capturedRoot"] = json.RawMessage(`"different"`)
	raw, err = json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if err := sameLossSaveEvidence(before, raw); err == nil {
		t.Fatal("replaced cut accepted")
	}
}
