//go:build linux

package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"google.golang.org/protobuf/proto"
)

type fileAgentProgramInput struct {
	files   map[string]string
	grant   *agentv1.SessionGrant
	release func(context.Context) (*agentv1.SessionGrant, error)
}

func (s *fileAgentProgramInput) copyArtifact(ctx context.Context, _ agentv1.SessionProgramRequest_Kind, d *agentv1.SessionProgramArtifact, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(s.files[d.Digest])
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.CopyN(w, f, d.SizeBytes)
	return err
}
func (s *fileAgentProgramInput) releaseStart(ctx context.Context) (*agentv1.SessionGrant, error) {
	if s.release != nil {
		return s.release(ctx)
	}
	return proto.Clone(s.grant).(*agentv1.SessionGrant), nil
}
func squashProgramFixture(t *testing.T, root string) (*agentv1.SessionProgramArtifact, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.sqfs")
	output, err := exec.Command("mksquashfs", root, path, "-noappend", "-all-root", "-no-progress", "-processors", "1").CombinedOutput()
	if err != nil {
		t.Fatalf("mksquashfs: %v %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return &agentv1.SessionProgramArtifact{Digest: sha256sum.DigestBytes(data), SizeBytes: int64(len(data))}, path
}

func nativeProgramInput(t *testing.T, grant *agentv1.SessionGrant, start *agentv1.SessionStart) *fileAgentProgramInput {
	t.Helper()
	runtime, runtimePath := squashProgramFixture(t, bootProgramMounts().Runtime)
	artifact, artifactPath := squashProgramFixture(t, bootProgramMounts().Artifact)
	start.Program = &agentv1.SessionProgram{Runtime: runtime, Artifact: artifact}
	return &fileAgentProgramInput{files: map[string]string{runtime.Digest: runtimePath, artifact.Digest: artifactPath}, grant: grant}
}

func TestSessionProgramImagesKeepIndependentReadOnlyBytes(t *testing.T) {
	if os.Getenv("HELMR_SESSION_PROGRAM_TESTS") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	roots := make([]string, 3)
	descriptors := make([]*agentv1.SessionProgramArtifact, 3)
	files := make(map[string]string)
	for i, value := range []string{"runtime", "one", "two"} {
		roots[i] = t.TempDir()
		if err := os.WriteFile(filepath.Join(roots[i], "value"), []byte(value), 0444); err != nil {
			t.Fatal(err)
		}
		d, path := squashProgramFixture(t, roots[i])
		descriptors[i] = d
		files[d.Digest] = path
	}
	store := &agentProgramStore{root: t.TempDir()}
	source := &fileAgentProgramInput{files: files}
	first, err := store.materialize(t.Context(), &agentv1.SessionProgram{Runtime: descriptors[0], Artifact: descriptors[1]}, source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.close(); err != nil {
			t.Error(err)
		}
	}()
	second, err := store.materialize(t.Context(), &agentv1.SessionProgram{Runtime: descriptors[0], Artifact: descriptors[2]}, source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.close(); err != nil {
			t.Error(err)
		}
	}()
	if first.runtime != second.runtime || first.artifact == second.artifact {
		t.Fatal("incorrect executable sharing")
	}
	for _, item := range []struct {
		lease *agentProgramLease
		value string
	}{{first, "one"}, {second, "two"}} {
		root := item.lease.mounts().Artifact
		data, err := os.ReadFile(filepath.Join(root, "value"))
		if err != nil || string(data) != item.value {
			t.Fatalf("Program bytes: %s %v", data, err)
		}
		if err := os.WriteFile(filepath.Join(root, "value"), []byte("changed"), 0644); err == nil {
			t.Fatal("Program mount is writable")
		}
		runProgramNamespaceProbe(t, item.lease.mounts(), item.value)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(second.mounts().Runtime, "value")); err != nil || string(data) != "runtime" {
		t.Fatalf("peer runtime removed: %s %v", data, err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(store.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Program resources leaked: %v %v", entries, err)
	}
}

func runProgramNamespaceProbe(t *testing.T, mounts programMounts, expected string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "workspace"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "tmp"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "tmp"), 0777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "probe"), data, 0555); err != nil {
		t.Fatal(err)
	}
	cmd, err := imageCommand(t.Context(), "/probe", []string{"-test.run=^TestSessionProgramNamespaceProbe$"}, "/workspace", []string{"HELMR_PROGRAM_PROBE=" + expected}, root, &resolvedRuntimeUser{UID: 1001, GID: 1001}, imageCommandOptions{Program: mounts})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private Program namespace: %v %s", err, output)
	}
}
func TestSessionProgramNamespaceProbe(t *testing.T) {
	expected := os.Getenv("HELMR_PROGRAM_PROBE")
	if expected == "" {
		t.Skip("namespace child only")
	}
	for _, item := range []struct{ path, want string }{{"/opt/helmr/program/value", expected}, {"/opt/helmr/runtime/value", "runtime"}} {
		data, err := os.ReadFile(item.path)
		if err != nil || string(data) != item.want {
			t.Fatalf("wrong pinned Program: %s %v", data, err)
		}
		if err := os.WriteFile(item.path, []byte("changed"), 0644); err == nil {
			t.Fatal("private Program mount is writable")
		}
	}
}

func TestSessionProgramRejectsWrongBytesAndReleasesReservation(t *testing.T) {
	if os.Getenv("HELMR_SESSION_PROGRAM_TESTS") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("runtime"), 0444); err != nil {
		t.Fatal(err)
	}
	descriptor, path := squashProgramFixture(t, root)
	bad := proto.Clone(descriptor).(*agentv1.SessionProgramArtifact)
	bad.Digest = "sha256:" + strings.Repeat("1", 64)
	store := &agentProgramStore{root: t.TempDir()}
	source := &fileAgentProgramInput{files: map[string]string{descriptor.Digest: path, bad.Digest: path}}
	if _, err := store.materialize(t.Context(), &agentv1.SessionProgram{Runtime: descriptor, Artifact: bad}, source); err == nil {
		t.Fatal("wrong bytes accepted")
	}
	entries, err := os.ReadDir(store.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed Program leaked: %v %v", entries, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.materialize(cancelled, &agentv1.SessionProgram{Runtime: descriptor, Artifact: descriptor}, source); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled materialization: %v", err)
	}
}

func TestSessionProgramImagesConcurrentStarts(t *testing.T) {
	if os.Getenv("HELMR_SESSION_PROGRAM_TESTS") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	const count = 8
	runtimeRoot := t.TempDir()
	runtime, runtimePath := squashProgramFixture(t, runtimeRoot)
	programs := make([]*agentv1.SessionProgram, count)
	files := map[string]string{runtime.Digest: runtimePath}
	for i := range count {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "identity"), []byte(fmt.Sprint(i)), 0600); err != nil {
			t.Fatal(err)
		}
		descriptor, path := squashProgramFixture(t, root)
		programs[i] = &agentv1.SessionProgram{Runtime: runtime, Artifact: descriptor}
		files[descriptor.Digest] = path
	}
	store := agentProgramStore{root: t.TempDir()}
	input := &fileAgentProgramInput{files: files}
	leases := make([]*agentProgramLease, count)
	start := make(chan struct{})
	var jobs sync.WaitGroup
	for i := range count {
		jobs.Go(func() {
			<-start
			var err error
			leases[i], err = store.materialize(t.Context(), programs[i], input)
			if err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	jobs.Wait()
	for i, lease := range leases {
		if lease == nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(lease.mounts().Artifact, "identity"))
		if err != nil || string(data) != fmt.Sprint(i) {
			t.Errorf("peer %d: %q %v", i, data, err)
		}
		jobs.Go(func() {
			if err := lease.close(); err != nil {
				t.Error(err)
			}
		})
	}
	jobs.Wait()
	entries, err := os.ReadDir(store.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("concurrent cleanup: %v %v", entries, err)
	}
}

func TestSessionProgramRechecksAdmissionAfterTransfer(t *testing.T) {
	if os.Getenv("HELMR_SESSION_PROGRAM_TESTS") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	for _, mode := range []string{"retired", "recovery fault"} {
		t.Run(mode, func(t *testing.T) {
			session, _ := agentSessionFixture(t)
			registry := newComputerOperationRegistry()
			registry.entries[session.entry.computerInstanceID] = session.entry
			registry.agentPrograms.root = t.TempDir()
			descriptor, path := squashProgramFixture(t, t.TempDir())
			start := proto.Clone(session.start).(*agentv1.SessionStart)
			start.Program = &agentv1.SessionProgram{Runtime: descriptor, Artifact: descriptor}
			source := &fileAgentProgramInput{files: map[string]string{descriptor.Digest: path}, grant: session.grant}
			source.release = func(context.Context) (*agentv1.SessionGrant, error) {
				if mode == "retired" {
					registry.retire(session.entry.computerInstanceID, session.entry)
				} else {
					session.entry.processesMu.Lock()
					session.entry.recoveryRequired = true
					session.entry.processesMu.Unlock()
				}
				return proto.Clone(session.grant).(*agentv1.SessionGrant), nil
			}
			relay, err := registry.openAgentSession(t.Context(), &agentv1.SessionAttach{Grant: session.grant, Start: start, AttachmentSequence: 1}, source)
			if relay != nil || err == nil || !strings.Contains(err.Error(), "superseded Session admission") {
				t.Fatalf("stale admission: %v %v", relay, err)
			}
			if len(registry.agentSessions) != 0 || len(registry.agentStarting) != 0 || session.entry.active != 0 {
				t.Fatal("rejected start retained ownership")
			}
			files, err := os.ReadDir(registry.agentPrograms.root)
			if err != nil || len(files) != 0 {
				t.Fatalf("rejected start retained images: %v %v", files, err)
			}
		})
	}
}
