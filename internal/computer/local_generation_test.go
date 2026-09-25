//go:build linux || darwin

package computer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"golang.org/x/sys/unix"
)

func localGenerationFixture(t *testing.T) (LocalGenerationConfig, string) {
	t.Helper()
	parent := t.TempDir()
	basePath := filepath.Join(parent, "base")
	store, err := cas.NewFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7().String()
	writer := blockformat.Writer{Source: store, Sink: store, Scope: "fixture", ActiveKey: key, Keys: map[string][]byte{key: bytes.Repeat([]byte{7}, 32)}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), 1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	locator, err = writer.Capture(t.Context(), locator, 1<<20, map[uint64][]byte{0: bytes.Repeat([]byte{1}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	root, err := NewGenerationRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return LocalGenerationConfig{Directory: filepath.Join(parent, "local"), Base: root, BaseSource: store, Scope: writer.Scope, ActiveKey: key, Keys: writer.Keys, DirtyBlocks: 8, StagedBytes: 32 << 20, PackLimit: blockformat.MinPackLimit}, basePath
}

func localByte(t *testing.T, p *LocalGeneration) byte {
	t.Helper()
	data := make([]byte, 1)
	if n, err := p.ReadAt(t.Context(), data, 7); err != nil || n != 1 {
		t.Fatalf("read: %d %v", n, err)
	}
	return data[0]
}

func TestLocalGenerationFlushReopenAndUnflushedClose(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenLocalGeneration(t.Context(), cfg); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal("concurrent owner accepted", err)
	}
	if _, err = p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	root, err := p.Flush(t.Context())
	if err != nil || root == cfg.Base {
		t.Fatalf("flush: %v", err)
	}
	if _, err = p.WriteAt(t.Context(), []byte{3}, 7); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if localByte(t, reopened) != 2 {
		t.Fatal("close persisted unflushed bytes or lost flush")
	}
	reopened.Close()
	changed := cfg
	changed.Scope = "different"
	if _, err = OpenLocalGeneration(t.Context(), changed); err == nil {
		t.Fatal("changed source scope adopted")
	}
	changed = cfg
	changed.StagedBytes = 1
	if _, err = OpenLocalGeneration(t.Context(), changed); !errors.Is(err, ErrGenerationStagingFull) {
		t.Fatal("reopen reset used staging bytes", err)
	}
	if _, err = CreateLocalGeneration(t.Context(), cfg); err == nil {
		t.Fatal("creation replaced local evidence")
	}
	if err = os.WriteFile(filepath.Join(cfg.Directory, "root"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenLocalGeneration(t.Context(), cfg); err == nil {
		t.Fatal("corrupt root silently reinitialized")
	}
}

func TestLocalGenerationFlushFailureCanRetry(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.WriteAt(t.Context(), []byte{4}, 7); err != nil {
		t.Fatal(err)
	}
	p.phase = func(phase string) error {
		if phase == "root-synced" {
			return errors.New("injected root write failure")
		}
		return nil
	}
	if _, err = p.Flush(t.Context()); err == nil {
		t.Fatal("failed root commit acknowledged")
	}
	p.phase = nil
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.Close()
	reopened, err := OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if localByte(t, reopened) != 4 {
		t.Fatal("retry lost captured changes")
	}
}

func TestLocalGenerationFlushOrder(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	p.phase = func(phase string) error {
		if phase == "root-synced" {
			calls++
			if calls == 1 {
				close(entered)
				select {
				case <-release:
				case <-time.After(5 * time.Second):
					return errors.New("test gate timeout")
				}
			}
		}
		return nil
	}
	if _, err = p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { _, e := p.Flush(t.Context()); done <- e }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not reach commit")
	}
	if _, err = p.WriteAt(t.Context(), []byte{3}, 7); err != nil {
		t.Fatal(err)
	}
	go func() { _, e := p.Flush(t.Context()); done <- e }()
	close(release)
	for range 2 {
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
	p.Close()
	reopened, err := OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if localByte(t, reopened) != 3 {
		t.Fatal("older flush replaced newer root")
	}
}

type localCrashConfig struct {
	Config          LocalGenerationConfig
	BasePath, Phase string
}

func runLocalHelper(t *testing.T, cfg LocalGenerationConfig, basePath, phase string) {
	t.Helper()
	cfg.BaseSource = nil
	raw, err := json.Marshal(localCrashConfig{cfg, basePath, phase})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLocalGenerationCrashHelper$")
	cmd.Env = append(os.Environ(), "HELMR_LOCAL_GENERATION_TEST="+string(raw))
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 42 {
		t.Fatalf("helper did not reach %s: %v %s", phase, err, output)
	}
}
func TestLocalGenerationCrashHelper(t *testing.T) {
	raw := os.Getenv("HELMR_LOCAL_GENERATION_TEST")
	if raw == "" {
		return
	}
	var cfg localCrashConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(cfg.BasePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Config.BaseSource = store
	p, err := OpenLocalGeneration(t.Context(), cfg.Config)
	if cfg.Phase == "locked" {
		if errors.Is(err, unix.EWOULDBLOCK) {
			os.Exit(42)
		}
		t.Fatal("process exclusion missing", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	p.phase = func(phase string) error {
		if phase == cfg.Phase {
			os.Exit(42)
		}
		return nil
	}
	if _, err = p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash boundary never reached")
}
func TestLocalGenerationProcessCrashBoundaries(t *testing.T) {
	for _, phase := range []string{"objects-staged", "root-synced", "root-renamed", "root-committed"} {
		t.Run(phase, func(t *testing.T) {
			cfg, basePath := localGenerationFixture(t)
			p, err := CreateLocalGeneration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "objects-staged" {
				runLocalHelper(t, cfg, basePath, "locked")
			}
			p.Close()
			runLocalHelper(t, cfg, basePath, phase)
			reopened, err := OpenLocalGeneration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			want := byte(1)
			if phase == "root-renamed" || phase == "root-committed" {
				want = 2
			}
			if got := localByte(t, reopened); got != want {
				t.Fatalf("crash recovered %d want %d", got, want)
			}
		})
	}
}

func TestLocalGenerationExhaustedBudgetReopens(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.WriteAt(t.Context(), []byte{2}, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.Close()
	var used int64
	err = filepath.WalkDir(filepath.Join(cfg.Directory, "objects"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err == nil {
			used += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.StagedBytes = used
	p, err = OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if localByte(t, p) != 2 {
		t.Fatal("exhausted reservation lost committed data")
	}
	if _, err = p.Flush(t.Context()); err != nil {
		t.Fatal("unchanged flush", err)
	}
	if _, err = p.WriteAt(t.Context(), []byte{3}, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Flush(t.Context()); !errors.Is(err, ErrGenerationStagingFull) {
		t.Fatal("new staging accepted", err)
	}
}

func TestLocalGenerationReclaimsCrashScratch(t *testing.T) {
	cfg, basePath := localGenerationFixture(t)
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	for range 3 {
		runLocalHelper(t, cfg, basePath, "root-synced")
		scratch := filepath.Join(cfg.Directory, "objects", ".object-abandoned")
		if err = os.WriteFile(scratch, []byte("incomplete"), 0600); err != nil {
			t.Fatal(err)
		}
		p, err = OpenLocalGeneration(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if localByte(t, p) != 1 {
			t.Fatal("uncommitted root adopted")
		}
		p.Close()
		err = filepath.WalkDir(cfg.Directory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if strings.HasPrefix(entry.Name(), ".root-") || strings.HasPrefix(entry.Name(), ".object-") {
				t.Errorf("abandoned scratch remains: %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalGenerationSourceLossStopsAllDiskOperations(t *testing.T) {
	for _, op := range []string{"read", "partial write", "partial trim", "flush"} {
		t.Run(op, func(t *testing.T) {
			cfg, base := localGenerationFixture(t)
			p, err := CreateLocalGeneration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if op == "flush" {
				if _, err = p.WriteAt(t.Context(), bytes.Repeat([]byte{2}, 4096), 0); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.Rename(base, base+".offline"); err != nil {
				t.Fatal(err)
			}
			switch op {
			case "read":
				_, err = p.ReadAt(t.Context(), make([]byte, 1), 7)
			case "partial write":
				_, err = p.WriteAt(t.Context(), []byte{2}, 7)
			case "partial trim":
				err = p.Trim(t.Context(), 7, 1)
			case "flush":
				_, err = p.Flush(t.Context())
			}
			var fatal *DeviceFailure
			var published *SourceFailure
			if !errors.As(err, &fatal) || !errors.Is(err, fs.ErrNotExist) || errors.As(err, &published) {
				t.Fatalf("unsafe source error: %v", err)
			}
			if err = os.Rename(base+".offline", base); err != nil {
				t.Fatal(err)
			}
		})
	}
}
