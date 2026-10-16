//go:build linux || darwin

package computerhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// This adapter keeps the actual owner/database operations while avoiding an
// unrelated HTTP fixture. The authenticated transport has separate coverage.
type pressureRetirementClient struct {
	AgentSaveClient
	fixture         sessiontest.Fixture
	publisher       *computer.SavePublisher
	host            workergroup.HostPrincipal
	identity        computer.LeaseIdentity
	captureRecorded chan struct{}
}

func (c *pressureRetirementClient) ref(s workerapi.AgentSave) computer.SavePublication {
	return computer.SavePublication{EnvironmentID: c.fixture.Environment, SaveID: uuid.MustParse(s.SaveID), LeaseEpoch: s.LeaseEpoch, Host: c.host}
}
func (c *pressureRetirementClient) NextAgentSave(ctx context.Context, r workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	var retirement *computer.SaveRetirement
	if r.Retirement != nil {
		retirement = &computer.SaveRetirement{SaveID: uuid.MustParse(r.Retirement.SaveID), Root: r.Retirement.Root}
	}
	save, err := computer.NextSave(ctx, c.fixture.Pool, c.host, c.identity, r.Admit, r.BackgroundDue, retirement)
	if err != nil || save == nil {
		return workerapi.AgentSavePending{}, err
	}
	if !save.Claimed {
		return workerapi.AgentSavePending{PendingForMillis: &save.AgeMillis}, nil
	}
	return workerapi.AgentSavePending{Save: &workerapi.AgentSave{EnvironmentID: c.fixture.Environment.String(), SaveID: save.ID.String(), LeaseEpoch: save.LeaseEpoch}, Sequence: save.Sequence}, nil
}
func (c *pressureRetirementClient) RegisterAgentSaveObject(ctx context.Context, r workerapi.AgentSaveObject) error {
	return c.publisher.RegisterBatch(ctx, c.ref(r.Save), r.Inspections)
}
func (c *pressureRetirementClient) CertifyAgentSaveObject(ctx context.Context, r workerapi.AgentSaveObject) error {
	return c.publisher.CertifyBatch(ctx, c.ref(r.Save), r.Inspections)
}
func (c *pressureRetirementClient) CaptureAgentSave(ctx context.Context, r workerapi.AgentSavePublication) error {
	if c.captureRecorded != nil {
		c.captureRecorded <- struct{}{}
	}
	return c.publisher.Capture(ctx, c.ref(r.Save), r.Root, r.Evidence)
}
func (c *pressureRetirementClient) PublishAgentSave(ctx context.Context, r workerapi.AgentSavePublication) error {
	return c.publisher.Publish(ctx, c.ref(r.Save), r.Root, r.Evidence)
}

type pressureRetirementReclaimer struct{ *cas.File }

func (pressureRetirementReclaimer) RetiredUploads(context.Context, string) ([]string, error) {
	return nil, nil
}
func (pressureRetirementReclaimer) ReclaimUpload(context.Context, string, string) error {
	return errors.New("file CAS has no multipart uploads")
}
func (s pressureRetirementReclaimer) ReclaimVersions(ctx context.Context, digest string) error {
	return s.Delete(ctx, digest)
}

// Barriers retain the real immutable pressure pin before and after each owned
// operation. They add no production protocol and allow real retention/sweep to
// run while the pressure lifetime is still active, including after local Release.
type pressureRetirementCapture struct {
	disk.RetainedVersion
	phases   chan string
	proceed  chan struct{}
	collects int
}

func (c *pressureRetirementCapture) pause(ctx context.Context, phase string) error {
	select {
	case c.phases <- phase:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-c.proceed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *pressureRetirementCapture) Collect(ctx context.Context, budget int) (int64, error) {
	c.collects++
	phase := "collect_before"
	if c.collects == 2 {
		phase = "collect_after"
	}
	if err := c.pause(ctx, phase); err != nil {
		return 0, err
	}
	return c.RetainedVersion.Collect(ctx, budget)
}
func (c *pressureRetirementCapture) Publish(ctx context.Context, p disk.ContinuationPublication) error {
	if err := c.pause(ctx, "publish"); err != nil {
		return err
	}
	return c.RetainedVersion.Publish(ctx, p)
}
func (c *pressureRetirementCapture) BackRemotely(ctx context.Context, budget int) error {
	if err := c.RetainedVersion.BackRemotely(ctx, budget); err != nil {
		return err
	}
	return c.pause(ctx, "backed")
}

type pressureRetirementMachine struct {
	localAgentSaveMachine
	captured chan struct{}
}

func (m pressureRetirementMachine) CaptureComputer(ctx context.Context) (*vm.ComputerSnapshot, error) {
	result, err := m.localAgentSaveMachine.CaptureComputer(ctx)
	close(m.captured)
	return result, err
}

func TestPressureLifetimeSerializesPublicationWhileRetirementSweeps(t *testing.T) {
	f := sessiontest.New(t)
	remote, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baseKey, writeKey, rootID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	keys := map[string][]byte{baseKey.String(): bytes.Repeat([]byte{7}, 32), writeKey.String(): bytes.Repeat([]byte{6}, 32)}
	writer := blockformat.Writer{Source: remote, Sink: remote, Scope: f.Environment.String(), ActiveKey: baseKey.String(), Keys: keys, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), 1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := blockformat.InspectPack(t.Context(), remote, writer.Scope, keys, locator.Pack)
	if err != nil {
		t.Fatal(err)
	}
	base, err := disk.NewVersionRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	rawRoot, _ := json.Marshal(base)
	rawInspection, _ := json.Marshal(blockformat.ObjectInspection{Pack: &inspected})
	digest, _ := base.Digest()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO disk_encryption_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,NULL,'test',decode('01','hex')),($3,$2,$4,'test',decode('02','hex'));
 INSERT INTO cas_blobs(digest,size_bytes) VALUES($5,$6);
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$5,$6,'application/octet-stream' FROM environments WHERE id=$2;
 INSERT INTO disk_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$5,org_id,project_id,$6,'application/octet-stream','root',$7,$8,clock_timestamp() FROM environments WHERE id=$2;
 INSERT INTO disk_object_keys(environment_id,digest,key_id,is_direct) VALUES($2,$5,$1,true);
 `+sessiontest.DiskRootInsert("$2", "$9", "$10")+`;
 UPDATE computers SET initial_root_id=$9,initial_root_digest=decode(substring($11::text from 8),'hex') WHERE environment_id=$2 AND id=$4;
 UPDATE computer_leases SET base_root_id=$9,write_key_id=$3 WHERE environment_id=$2 AND computer_id=$4 AND epoch=1`, pgx.QueryExecModeSimpleProtocol, baseKey, f.Environment, writeKey, f.Computer, base.Pack.Digest, base.Pack.SizeBytes, base.Pack.Rank, string(rawInspection), rootID, string(rawRoot), digest)
	publisher, err := computer.NewSavePublisher(f.Pool, remote)
	if err != nil {
		t.Fatal(err)
	}
	client := &pressureRetirementClient{fixture: f, publisher: publisher, host: workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}, identity: computer.LeaseIdentity{EnvironmentID: f.Environment, ComputerID: f.Computer, InstanceID: f.Computer, Epoch: 1}}
	cfg := disk.LocalVersionConfig{Directory: filepath.Join(t.TempDir(), "local"), Base: base, BaseSource: remote, Scope: writer.Scope, ActiveKey: writeKey.String(), Keys: keys, DirtyBlocks: 8, StagedBytes: 128 << 10, PackLimit: blockformat.MinPackLimit}
	local, err := disk.CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	allocation := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: f.Environment.String(), OwnerID: f.Computer.String(), AllocationID: f.Computer.String(), Epoch: 1}
	var oldRoots []disk.VersionRoot
	for _, value := range []byte{1, 2} {
		next, err := client.NextAgentSave(t.Context(), workerapi.AgentSaveDiscovery{AllocationIdentity: allocation, Admit: true, BackgroundDue: true})
		if err != nil || next.Save == nil {
			t.Fatal(err)
		}
		if _, err = local.WriteAt(t.Context(), bytes.Repeat([]byte{value}, 4096), 0); err != nil {
			t.Fatal(err)
		}
		capture, err := local.Capture(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		receipt := workerapi.AgentSavePublication{Save: *next.Save, Root: capture.Root(), Evidence: "ordered local cut"}
		if err = client.CaptureAgentSave(t.Context(), receipt); err != nil {
			t.Fatal(err)
		}
		if err = capture.Publish(t.Context(), agentSavePublisher{client: client, objects: saveStoragePublisher{remote}, save: *next.Save}); err != nil {
			t.Fatal(err)
		}
		if err = client.PublishAgentSave(t.Context(), receipt); err != nil {
			t.Fatal(err)
		}
		if err = capture.Adopt(t.Context()); err != nil {
			t.Fatal(err)
		}
		capture.Release()
		oldRoots = append(oldRoots, capture.Root())
		if value == 2 {
			if !capture.ReadyForRetirement() {
				t.Fatal("published floor not ready")
			}
			if _, err = client.NextAgentSave(t.Context(), workerapi.AgentSaveDiscovery{AllocationIdentity: allocation, Retirement: &workerapi.AgentSaveRetirement{SaveID: next.Save.SaveID, Root: capture.Root()}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, root := range oldRoots {
		if _, err := remote.Stat(t.Context(), root.Pack.Digest); err != nil {
			t.Fatalf("historical payload was never uploaded: %v", err)
		}
	}
	// Actual local staging exhaustion starts pressure. A delayed diagnostic keeps
	// this one pressure attempt active even if its first collection frees space.
	lastValue := byte(2)
	for value := byte(3); ; value++ {
		if value == 100 {
			t.Fatal("fixture did not fill staging")
		}
		_, err = local.WriteAt(t.Context(), bytes.Repeat([]byte{value}, 4096), 0)
		if errors.Is(err, disk.ErrVersionStagingFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = local.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		lastValue = value
	}
	if local.Changes().PressureSince.IsZero() {
		t.Fatal("fixture did not enter pressure")
	}
	next, err := client.NextAgentSave(t.Context(), workerapi.AgentSaveDiscovery{AllocationIdentity: allocation, Admit: true, BackgroundDue: true})
	if err != nil || next.Save == nil {
		t.Fatal(err)
	}
	phases, proceed := make(chan string), make(chan struct{})
	machineCaptured := make(chan struct{})
	s := &agentComputerSaves{client: client, objects: saveStoragePublisher{remote}, identity: allocation, machine: pressureRetirementMachine{localAgentSaveMachine: localAgentSaveMachine{local: local, computer: allocation.OwnerID}, captured: machineCaptured}, changes: func() disk.VersionChanges {
		state := local.Changes()
		state.PressureSince = time.Unix(1, 0)
		return state
	}}
	s.pinCommitted = func(ctx context.Context) (disk.RetainedVersion, error) {
		capture, err := local.PinCommitted(ctx)
		if err != nil {
			return nil, err
		}
		return &pressureRetirementCapture{RetainedVersion: capture, phases: phases, proceed: proceed}, nil
	}
	s.setPressureSave(next.Save)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pressureDone := make(chan error, 1)
	go func() { pressureDone <- s.relievePressure(ctx) }()
	sweepStore := pressureRetirementReclaimer{remote}
	reclaimer, err := computer.NewCASReclaimer(f.Pool, sweepStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	sweep := func() {
		t.Helper()
		for range 8 {
			if err := computer.ReconcileDiskRetention(ctx, f.Pool); err != nil {
				t.Fatal(err)
			}
			if err := reclaimer.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	mainDone := make(chan error, 1)
	client.captureRecorded = make(chan struct{}, 1)
	for i, wantPhase := range []string{"collect_before", "publish", "backed", "collect_after"} {
		select {
		case phase := <-phases:
			if phase != wantPhase {
				t.Fatalf("phase %q want %q", phase, wantPhase)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if i == 1 {
			go func() { mainDone <- s.step(ctx) }()
			select {
			case <-machineCaptured:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		// The main immutable capture can finish, but cannot record coverage before
		// the pressure owner joins publication, backing and final collection.
		select {
		case <-client.captureRecorded:
			t.Fatal("main publication outran pressure lifetime")
		default:
		}
		sweep()
		if _, err := remote.Stat(ctx, oldRoots[0].Pack.Digest); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("safe old payload was not physically swept during %s: %v", wantPhase, err)
		}
		if _, err := remote.Stat(ctx, oldRoots[1].Pack.Digest); err != nil {
			t.Fatalf("pressure lifetime lost recovery root during %s: %v", wantPhase, err)
		}
		var requested bool
		if err := f.Pool.QueryRow(ctx, `SELECT status='requested' FROM computer_saves WHERE id=$1`, next.Save.SaveID).Scan(&requested); err != nil || !requested {
			t.Fatalf("pressure published coverage at %s: %v %v", wantPhase, requested, err)
		}
		proceed <- struct{}{}
	}
	if err := <-pressureDone; err != nil {
		t.Fatal(err)
	}
	if err := <-mainDone; err != nil {
		t.Fatal(err)
	}
	// The delayed pressure attempt has joined. Resume observing the actual
	// device instead of the forced pressure used to hold that lifetime open.
	s.changes = local.Changes
	if err := s.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	s.prepareRetirement()
	if s.retirement == nil {
		t.Fatal("joined pressure lifetime did not allow next adopted floor")
	}
	if _, err := client.NextAgentSave(ctx, workerapi.AgentSaveDiscovery{AllocationIdentity: allocation, Retirement: s.retirement}); err != nil {
		t.Fatal(err)
	}
	sweep()
	if _, err := remote.Stat(ctx, oldRoots[1].Pack.Digest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("obsolete recovery bytes survived next floor: %v", err)
	}
	restored, err := disk.OpenVersion(ctx, remote, cfg.Scope, cfg.Keys, s.retirement.Root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	data, err := restored.ReadRange(ctx, 0, 4096)
	if err != nil || !bytes.Equal(data, bytes.Repeat([]byte{lastValue}, 4096)) {
		t.Fatalf("pressure-preserved bytes did not restore: %v", err)
	}
	s.release()
}
