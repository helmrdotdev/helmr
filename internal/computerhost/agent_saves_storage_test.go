//go:build linux || darwin

package computerhost

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"io"
	"path/filepath"
	"testing"
	"uuid"
)

type localAgentSaveMachine struct {
	liveCaptureMachine
	local    *disk.LocalVersion
	computer string
	wrap     func(*disk.LocalCapture) computerSaveCapture
}

func (m localAgentSaveMachine) CaptureComputer(ctx context.Context) (*vm.ComputerSnapshot, error) {
	capture, err := m.local.Capture(ctx)
	if err != nil {
		return nil, err
	}
	if m.wrap != nil {
		return &vm.ComputerSnapshot{ComputerID: m.computer, Capture: m.wrap(capture)}, nil
	}
	return &vm.ComputerSnapshot{ComputerID: m.computer, Capture: capture}, nil
}
func (c *agentSaveTestClient) RegisterAgentSaveObject(context.Context, workerapi.AgentSaveObject) error {
	return nil
}
func (c *agentSaveTestClient) CertifyAgentSaveObject(context.Context, workerapi.AgentSaveObject) error {
	return nil
}

func TestAgentSaveRepeatedCutsReuseStagingBudget(t *testing.T) {
	remote, err := cas.NewFile(filepath.Join(t.TempDir(), "remote"))
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7().String()
	writer := blockformat.Writer{Source: remote, Sink: remote, Scope: "fixture", ActiveKey: key, Keys: map[string][]byte{key: bytes.Repeat([]byte{7}, 32)}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), 1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	source := &saveRangeOutage{File: remote}
	cfg := disk.LocalVersionConfig{Directory: filepath.Join(t.TempDir(), "local"), Base: root, BaseSource: source, Scope: writer.Scope, ActiveKey: key, Keys: writer.Keys, DirtyBlocks: 8, StagedBytes: 96 << 10, PackLimit: blockformat.MinPackLimit}
	local, err := disk.CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	s, c := agentSaveFixture(t, "")
	s.objects = saveStoragePublisher{remote}
	machine := localAgentSaveMachine{local: local, computer: s.identity.OwnerID}
	s.machine = machine
	// The CP stub accepts identities; actual ciphertext publication, source
	// adoption and reclamation run through the production save coordinator.
	for i := 1; i <= 32; i++ {
		c.save.SaveID = uuid.NewV7().String()
		stage := ""
		if i == 2 {
			stage = "adopt"
		}
		if i == 3 {
			stage = "collect"
		}
		machine.wrap = func(c *disk.LocalCapture) computerSaveCapture {
			return &localSaveReadOutage{LocalCapture: c, source: source, stage: stage}
		}
		s.machine = machine
		want := bytes.Repeat([]byte{byte(i)}, 4096)
		if _, err := local.WriteAt(t.Context(), want, 4096); err != nil {
			t.Fatal(err)
		}
		if err := s.step(t.Context()); stage != "" {
			if !errors.Is(err, cas.ErrUnavailable) || s.pending == nil {
				t.Fatalf("%s read outage did not retain cut: %v", stage, err)
			}
			pending := *s.pending
			if err := s.step(t.Context()); err != nil {
				t.Fatalf("retry %s: %v", stage, err)
			}
			if s.pending != nil || len(c.requests) == 0 || c.requests[len(c.requests)-1] != pending {
				t.Fatal("read outage changed published cut")
			}
		} else if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		actual := make([]byte, 4096)
		if _, err := local.ReadAt(t.Context(), actual, 4096); err != nil || !bytes.Equal(actual, want) {
			t.Fatalf("read adopted cut %d: %v", i, err)
		}
	}
}

type saveRangeOutage struct {
	*cas.File
	fail bool
}

func (s *saveRangeOutage) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	if s.fail {
		s.fail = false
		return nil, cas.ErrUnavailable
	}
	return s.File.GetRange(ctx, digest, size, offset, length)
}

type localSaveReadOutage struct {
	*disk.LocalCapture
	source *saveRangeOutage
	stage  string
}

func (c *localSaveReadOutage) Adopt(ctx context.Context, limit int) error {
	if c.stage == "adopt" {
		c.source.fail = true
		c.stage = ""
	}
	return c.LocalCapture.Adopt(ctx, limit)
}
func (c *localSaveReadOutage) Collect(ctx context.Context, limit int) (int64, error) {
	if c.stage == "collect" {
		c.source.fail = true
		c.stage = ""
	}
	return c.LocalCapture.Collect(ctx, limit)
}
