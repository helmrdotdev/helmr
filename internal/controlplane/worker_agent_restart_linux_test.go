//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bundle"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

type nativeControlPlaneInput struct {
	DSN    string
	Key    []byte
	Config nativeExecutionConfig
}

// This child runs the real publication service and its production lifecycle
// owners. It is a test executable composition, not the deployment entry point.
// PostgreSQL, object storage and the live Worker remain outside its OS lifetime.
func TestNativeControlPlaneProcess(t *testing.T) {
	if os.Getenv("HELMR_NATIVE_CP_CHILD") != "1" {
		t.Skip("owned native proof child")
	}
	var input nativeControlPlaneInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pool, err := pgxpool.New(ctx, input.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)
	logFile := os.Stdout
	secrets, err := secret.New(q, pool, input.Key)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := computerkey.NewLocal("native-execution-fixture", input.Key)
	if err != nil {
		t.Fatal(err)
	}
	fencing, err := disk.NewFencingKey(input.Key)
	if err != nil {
		t.Fatal(err)
	}
	allocator, err := agent.NewAllocator(pool, input.Key, secrets)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cass3.New(ctx, input.Config.CASURI)
	if err != nil {
		t.Fatal(err)
	}
	platform, err := cass3.NewImmutable(ctx, input.Config.PlatformURI)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRaw, err := os.ReadFile(input.Config.RuntimeDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := artifact.ParseRuntimeDescriptor(runtimeRaw)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(logFile, nil))
	cfg := completeServerConfig(t)
	cfg.DB, cfg.TX, cfg.DiagnosticDB, cfg.ReadinessDB = q, pool, pool, pool
	cfg.Auth, cfg.Allocator, cfg.Log = identity.NewAPIKeyAuthenticator(q), allocator, log
	cfg.ComputerKeys, cfg.ComputerFencingKey = keys, fencing
	cfg.CAS, cfg.PlatformStore, cfg.BundleAdmission = nativeFaultStore{store}, platform, bundle.Admission{Runtime: runtime}
	cfg.Secrets, cfg.SecretDelivery, cfg.SecretProxy = secrets, secrets, secrets
	cfg.DiagnosticBounds = diagnostic.Bounds{ChunkBytes: 1 << 20, SourceBytes: 32 << 20, SourceRecords: 32768, EnvironmentBytes: 128 << 20, EnvironmentRecords: 131072, QueueBytes: 128 << 20, QueueRecords: 131072}
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listenerFile := os.NewFile(3, "control-plane-listener")
	listener, err := net.FileListener(listenerFile)
	listenerFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: nativeStorageFaultHandler(handler), ReadHeaderTimeout: 10 * time.Second}
	var jobs sync.WaitGroup
	reclaimer, err := agent.NewCASReclaimer(pool, store, log)
	if err != nil {
		t.Fatal(err)
	}
	// Complete a real retention/reclamation pass before readiness on every
	// incarnation, and continue the normal loops while requests execute.
	if err := agent.ReconcileComputerDiskRetention(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := reclaimer.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	failures := make(chan error, 8)
	for _, run := range []func(context.Context) error{
		reclaimer.Run,
		func(ctx context.Context) error { return agent.RunComputerDiskRetention(ctx, pool, log) },
		func(ctx context.Context) error { return allocator.Run(ctx, log) },
		func(ctx context.Context) error { return agent.RunPreparationLifecycle(ctx, pool, log) },
		func(ctx context.Context) error { return agent.RunSessionLifecycle(ctx, pool, log) },
		func(ctx context.Context) error { return agent.RunComputerLifecycle(ctx, pool, log) },
		func(ctx context.Context) error { return command.ReconcileCommands(ctx, pool, log) },
		func(context.Context) error { return server.Serve(listener) },
	} {
		jobs.Go(func() { failures <- run(ctx) })
	}
	defer func() { cancel(); server.Close(); jobs.Wait() }()
	ready := os.NewFile(4, "control-plane-ready")
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	ready.Close()
	select {
	case err := <-failures:
		t.Fatalf("publication owner stopped: %v", err)
	case <-ctx.Done():
	}
}

type nativeControlPlaneProcess struct {
	mu       sync.Mutex
	ctx      context.Context
	input    []byte
	casURI   string
	listener *os.File
	log      *os.File
	child    *nativeControlPlaneChild
	fail     func(error)
	proxy    *httputil.ReverseProxy
}

func startNativeControlPlane(t *testing.T, cancelWork context.CancelFunc, input nativeControlPlaneInput) *nativeControlPlaneProcess {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	listenerFile, err := listener.File()
	address := listener.Addr().String()
	listener.Close()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(input.Config.Evidence+"/control-plane.log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		listenerFile.Close()
		t.Fatal(err)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		log.Close()
		listenerFile.Close()
		t.Fatal(err)
	}
	target := &url.URL{Scheme: "http", Host: address}
	process := &nativeControlPlaneProcess{ctx: t.Context(), fail: func(err error) { t.Errorf("publication owner: %v", err); cancelWork() }, input: raw, casURI: input.Config.CASURI, listener: listenerFile, log: log, proxy: httputil.NewSingleHostReverseProxy(target)}
	process.proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "publication owner unavailable", http.StatusServiceUnavailable)
	}
	t.Cleanup(process.close)
	if err := process.start(); err != nil {
		t.Fatal(err)
	}
	return process
}

func (p *nativeControlPlaneProcess) start() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ready, readyWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer ready.Close()
	defer readyWriter.Close()
	cmd := exec.Command(executable, "-test.run=^TestNativeControlPlaneProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "HELMR_NATIVE_CP_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.Stdin = bytes.NewReader(p.input)
	cmd.Stdout, cmd.Stderr = p.log, p.log
	cmd.ExtraFiles = []*os.File{p.listener, readyWriter}
	if err := cmd.Start(); err != nil {
		return err
	}
	p.observeChild(cmd)
	readyWriter.Close()
	acknowledged := make(chan error, 1)
	go func() { var value [1]byte; _, err := io.ReadFull(ready, value[:]); acknowledged <- err }()
	select {
	case err := <-acknowledged:
		if err == nil {
			return nil
		}
		p.stop()
		return fmt.Errorf("publication owner startup: %w", err)
	case <-time.After(10 * time.Second):
		p.stop()
		return errors.New("publication owner startup timed out")
	case <-p.ctx.Done():
		p.stop()
		return p.ctx.Err()
	}
}

func (p *nativeControlPlaneProcess) observeChild(cmd *exec.Cmd) {
	child := &nativeControlPlaneChild{command: cmd, done: make(chan struct{})}
	p.child = child
	go func() {
		child.err = cmd.Wait()
		if !child.stopping.Load() {
			p.fail(fmt.Errorf("unexpected child exit: %v", child.err))
		}
		close(child.done)
	}()
}

type nativeControlPlaneChild struct {
	command  *exec.Cmd
	done     chan struct{}
	stopping atomic.Bool
	err      error
}

func (p *nativeControlPlaneProcess) stop() error {
	child := p.child
	if child == nil {
		return nil
	}
	child.stopping.Store(true)
	killErr := child.command.Process.Kill()
	<-child.done
	p.child = nil
	var exit *exec.ExitError
	if killErr != nil || !errors.As(child.err, &exit) {
		return fmt.Errorf("injected termination not verified: kill=%v wait=%v", killErr, child.err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		return fmt.Errorf("unexpected child termination: %v", child.err)
	}
	return nil
}

func (p *nativeControlPlaneProcess) restart() (int, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.child == nil {
		return 0, 0, errors.New("publication owner is not running")
	}
	oldPID := p.child.command.Process.Pid
	if err := p.stop(); err != nil {
		return oldPID, 0, err
	}
	if err := p.start(); err != nil {
		return oldPID, 0, err
	}
	if p.child.command.Process.Pid == oldPID {
		return oldPID, oldPID, errors.New("publication owner PID did not change")
	}
	return oldPID, p.child.command.Process.Pid, nil
}

func (p *nativeControlPlaneProcess) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.stop(); err != nil {
		p.fail(err)
	}
	p.listener.Close()
	p.log.Close()
}

type nativePublicationRestart struct {
	LocalBefore, LocalAfter                     nativeLocalRetention
	TerminalBeforeRestart, TerminalAfterRestart *time.Time
	BeforePublicationCommit                     bool
	OldPID, NewPID                              int
	StartedAt, ReadyAt, LeaseExpiresAt          time.Time
	Before, After                               nativeSaveRestartIdentity
}

type nativeSaveRestartIdentity struct {
	PinnedObjectBytes, CertifiedPinnedObjectBytes                               int64
	Computer, Instance, Host, CapturedRoot, CaptureEvidence, Result, ObjectPins string
	BaseRoot                                                                    string
	LeaseEpoch, WorkerEpoch                                                     int64
	CapturedAt                                                                  time.Time
}

func (g *nativePublicationGate) restartIdentity(ctx context.Context, save string, requireObjects bool) (nativeSaveRestartIdentity, time.Time, string, error) {
	var identity nativeSaveRestartIdentity
	var expires time.Time
	var state string
	var valid bool
	err := g.pool.QueryRow(ctx, `SELECT s.computer_id::text,l.computer_instance_id::text,l.worker_host_id::text,
 encode(s.captured_root_digest,'hex'),s.capture_evidence,t.result::text,l.epoch,l.worker_epoch,s.captured_at,l.expires_at,s.status,
 COALESCE((SELECT jsonb_agg(p.digest ORDER BY p.digest)::text FROM computer_object_pins p WHERE p.environment_id=s.environment_id AND p.save_id=s.id),'[]'),
 COALESCE((SELECT sum(o.size_bytes) FROM computer_object_pins p JOIN computer_objects o ON (o.environment_id,o.digest)=(p.environment_id,p.digest) WHERE p.environment_id=s.environment_id AND p.save_id=s.id),0),
 COALESCE((SELECT sum(o.size_bytes) FROM computer_object_pins p JOIN computer_objects o ON (o.environment_id,o.digest)=(p.environment_id,p.digest) WHERE p.environment_id=s.environment_id AND p.save_id=s.id AND o.certified),0),
 l.base_root_id::text, l.retained_base_root_id IS NOT NULL AND l.status IN ('active','releasing') AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp() AND h.current_epoch=l.worker_epoch
 FROM computer_saves s JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(s.environment_id,s.computer_id,s.computer_lease_epoch)
 JOIN worker_hosts h ON h.id=l.worker_host_id JOIN turns t ON (t.environment_id,t.id)=(s.environment_id,s.turn_id)
 WHERE s.environment_id=$1 AND s.id=$2`, g.environment, save).Scan(&identity.Computer, &identity.Instance, &identity.Host, &identity.CapturedRoot, &identity.CaptureEvidence, &identity.Result, &identity.LeaseEpoch, &identity.WorkerEpoch, &identity.CapturedAt, &expires, &state, &identity.ObjectPins, &identity.PinnedObjectBytes, &identity.CertifiedPinnedObjectBytes, &identity.BaseRoot, &valid)
	if err == nil && !valid {
		err = errors.New("publication owner restart lost live writer authority")
	}
	if err == nil && requireObjects && identity.ObjectPins == "[]" {
		err = errors.New("publication owner restart lost save object pins")
	}
	return identity, expires, state, err
}

func (g *nativePublicationGate) restartPublication(w http.ResponseWriter, r *http.Request, save string) error {
	g.restartMu.Lock()
	defer g.restartMu.Unlock()
	g.mu.Lock()
	observation := g.observed[save]
	g.mu.Unlock()
	proof := nativePublicationRestart{BeforePublicationCommit: observation.TurnSequence == 1}
	var state string
	var err error
	proof.Before, proof.LeaseExpiresAt, state, err = g.restartIdentity(r.Context(), save, true)
	if err != nil {
		return err
	}
	if state != "captured" {
		return fmt.Errorf("before restart publication state %s", state)
	}
	if !proof.BeforePublicationCommit {
		// Commit through the actual service, but discard its reply to the Worker.
		response := httptest.NewRecorder()
		g.next.ServeHTTP(response, r)
		if response.Code != http.StatusNoContent {
			return fmt.Errorf("publication before lost reply returned %d", response.Code)
		}
	}
	want := "published"
	if proof.BeforePublicationCommit {
		want = "captured"
	}
	if err := g.restartSavedCut(r.Context(), save, &proof, want, true); err != nil {
		return err
	}

	g.mu.Lock()
	observation = g.observed[save]
	observation.Restart = &proof
	g.observed[save] = observation
	g.mu.Unlock()
	http.Error(w, "publication owner restarted; retry same operation", http.StatusServiceUnavailable)
	return nil
}

func (g *nativePublicationGate) restartSavedCut(ctx context.Context, save string, proof *nativePublicationRestart, want string, requireObjects bool) error {
	var err error
	var state string
	if err := g.pool.QueryRow(ctx, `SELECT terminal_at FROM turns WHERE environment_id=$1 AND id=(SELECT turn_id FROM computer_saves WHERE environment_id=$1 AND id=$2)`, g.environment, save).Scan(&proof.TerminalBeforeRestart); err != nil {
		return err
	}
	proof.LocalBefore, err = g.localRetention(proof.Before)
	if err != nil {
		return err
	}
	proof.StartedAt = time.Now().UTC()
	proof.OldPID, proof.NewPID, err = g.restart.restart()
	proof.ReadyAt = time.Now().UTC()
	if err != nil {
		return err
	}
	if !proof.ReadyAt.Before(proof.LeaseExpiresAt) {
		return errors.New("restart exceeded pre-fault lease expiry")
	}
	proof.After, _, state, err = g.restartIdentity(ctx, save, requireObjects)
	if err != nil {
		return err
	}
	if state != want || proof.Before != proof.After {
		return fmt.Errorf("restart changed save identity or disposition: state %s want %s", state, want)
	}
	if err := g.pool.QueryRow(ctx, `SELECT terminal_at FROM turns WHERE environment_id=$1 AND id=(SELECT turn_id FROM computer_saves WHERE environment_id=$1 AND id=$2)`, g.environment, save).Scan(&proof.TerminalAfterRestart); err != nil {
		return err
	}
	proof.LocalAfter, err = g.localRetention(proof.After)
	if err != nil {
		return err
	}
	return nil
}

func (g *nativePublicationGate) awaitReconciliation(ctx context.Context, saves []string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		g.mu.Lock()
		err := errors.Join(g.errors...)
		complete := true
		for _, save := range saves {
			observation, exists := g.observed[save]
			complete = complete && exists && observation.Restart != nil && !observation.ReconciledAt.IsZero()
			if g.restart != nil {
				early := observation.EarlyRecovery
				complete = complete && early != nil && early.Capture.Restart != nil && early.PartialUpload.Restart != nil &&
					!early.Capture.ReconciledAt.IsZero() && !early.PartialUpload.ReconciledAt.IsZero()
			}
		}
		g.mu.Unlock()
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestNativeRestartRequiresSuccessfulReconciliation(t *testing.T) {
	save := uuid.NewV7().String()
	gate := nativePublicationGate{
		observed: map[string]nativePublicationObservation{save: {PublicationAttempts: 1, Restart: &nativePublicationRestart{}}},
		next:     http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }),
	}
	body, err := json.Marshal(workerapi.AgentSavePublication{Save: workerapi.AgentSave{SaveID: save}})
	if err != nil {
		t.Fatal(err)
	}
	gate.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/publish", bytes.NewReader(body)))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := gate.awaitReconciliation(ctx, []string{save}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed retry satisfied proof: %v", err)
	}
	gate.next = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	gate.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/worker/v1/computer-saves/publish", bytes.NewReader(body)))
	if err := gate.awaitReconciliation(t.Context(), []string{save}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeControlPlaneChildExitIsObserved(t *testing.T) {
	for _, unexpected := range []bool{false, true} {
		name, script := "injected-kill", "exec sleep 60"
		if unexpected {
			name, script = "unexpected-exit", "exit 7"
		}
		t.Run(name, func(t *testing.T) {
			failures := make(chan error, 1)
			p := nativeControlPlaneProcess{fail: func(err error) { failures <- err }}
			cmd := exec.Command("/bin/sh", "-c", script)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			p.observeChild(cmd)
			if unexpected {
				select {
				case <-p.child.done:
				case <-time.After(5 * time.Second):
					_ = cmd.Process.Kill()
					t.Fatal("child exit not observed")
				}
				select {
				case <-failures:
				default:
					t.Fatal("unexpected child exit was discarded")
				}
				if err := p.stop(); err == nil {
					t.Fatal("unexpected exit accepted as injected kill")
				}
			} else {
				if err := p.stop(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-failures:
					t.Fatalf("injected stop reported unexpected: %v", err)
				default:
				}
			}
		})
	}
}

func TestNativeRestartRequiresBothEarlyStages(t *testing.T) {
	save := uuid.NewV7().String()
	done := nativeRecoveryStage{Restart: &nativePublicationRestart{}, ReconciledAt: time.Now().UTC()}
	for _, tc := range []struct {
		name     string
		early    *nativeEarlyRecovery
		complete bool
	}{
		{"missing", nil, false},
		{"capture-only", &nativeEarlyRecovery{Capture: done}, false},
		{"upload-not-reconciled", &nativeEarlyRecovery{Capture: done, PartialUpload: nativeRecoveryStage{Restart: &nativePublicationRestart{}}}, false},
		{"both-reconciled", &nativeEarlyRecovery{Capture: done, PartialUpload: done}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := nativePublicationGate{restart: &nativeControlPlaneProcess{}, observed: map[string]nativePublicationObservation{save: {Restart: &nativePublicationRestart{}, ReconciledAt: time.Now().UTC(), EarlyRecovery: tc.early}}}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			err := gate.awaitReconciliation(ctx, []string{save})
			if tc.complete && err != nil || !tc.complete && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("incomplete recovery accepted or complete recovery rejected: %v", err)
			}
		})
	}
}

// These scans account regular files without following symlinks or counting a
// hard-linked inode twice. They are observations while the peer keeps writing,
// not atomic snapshots, high-water marks or an assertion of unchanged live bytes.
type nativeByteScan struct {
	ObservedAt                   time.Time
	DurationMS                   float64
	Files, VanishedFiles         int64
	LogicalBytes, AllocatedBytes int64
}
type nativeLocalRetention struct{ Arena, WorkerTemporary nativeByteScan }

func (g *nativePublicationGate) localRetention(identity nativeSaveRestartIdentity) (nativeLocalRetention, error) {
	var result nativeLocalRetention
	arena := filepath.Join(g.workRoot, "tmp", fmt.Sprintf("computer-%s-%d", identity.Instance, identity.LeaseEpoch))
	var err error
	result.Arena, err = scanNativeBytes(arena)
	if err != nil {
		return result, err
	}
	result.WorkerTemporary, err = scanNativeBytes(filepath.Join(g.workRoot, "tmp"))
	return result, err
}

func scanNativeBytes(root string) (nativeByteScan, error) {
	started := time.Now()
	result := nativeByteScan{ObservedAt: started.UTC()}
	seen := map[[2]uint64]struct{}{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path != root && errors.Is(err, os.ErrNotExist) {
				result.VanishedFiles++
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			result.VanishedFiles++
			return nil
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("local retention stat unavailable")
		}
		id := [2]uint64{uint64(stat.Dev), stat.Ino}
		if _, ok := seen[id]; ok {
			return nil
		}
		seen[id] = struct{}{}
		result.Files++
		result.LogicalBytes += info.Size()
		result.AllocatedBytes += stat.Blocks * 512
		return nil
	})
	result.DurationMS = float64(time.Since(started)) / float64(time.Millisecond)
	return result, err
}

func TestNativeByteScanCountsAllocatedCustody(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("retained"), 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "sparse"), filepath.Join(root, "same-inode")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	result, err := scanNativeBytes(root)
	if err != nil || result.Files != 1 || result.LogicalBytes != 1<<20 || result.AllocatedBytes <= 0 || result.VanishedFiles != 0 {
		t.Fatalf("local custody scan: %+v %v", result, err)
	}
}
