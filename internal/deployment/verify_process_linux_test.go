//go:build linux

package deployment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if handled, err := RunVerifierChild(os.Args); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, VerifierChildLocalDiagnostic(err))
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestVerifierCommandProtocolUsesRealProcessBoundary(t *testing.T) {
	for _, test := range []struct {
		mode             string
		wantBootstrap    bool
		wantDiagnostic   string
		wantResult       verifierRecordKind
		forbidDiagnostic string
	}{
		{
			mode: "ready-invalid", wantResult: verifierInvalid,
			wantDiagnostic: "pre-ready diagnostic", forbidDiagnostic: "post-ready tenant sentinel",
		},
		{
			mode: "graceful-pre-ready", wantBootstrap: true,
			wantDiagnostic: "graceful pre-ready failure",
		},
		{
			mode: "abort-pre-ready", wantBootstrap: true,
			wantDiagnostic: "abort pre-ready failure",
		},
	} {
		t.Run(test.mode, func(t *testing.T) {
			resultReader, resultWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer resultReader.Close()
			defer resultWriter.Close()
			capture, err := newVerifierBootstrapCapture()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVerifierProtocolHelper$")
			command.Env = append(os.Environ(), "HELMR_VERIFIER_PROTOCOL_HELPER="+test.mode)
			command.ExtraFiles = []*os.File{resultWriter}
			command.Stderr = capture.writer
			stop := func() {
				_ = resultReader.Close()
				if command.Process != nil {
					_ = command.Process.Kill()
				}
			}
			result, err := runVerifierCommandProtocol(
				ctx, command, resultReader, resultWriter, capture,
				runtimeVerifierJob, stop,
			)
			var bootstrap *verifierBootstrapError
			if errors.As(err, &bootstrap) != test.wantBootstrap {
				t.Fatalf("protocol error = %v, want bootstrap %t", err, test.wantBootstrap)
			}
			if !test.wantBootstrap && err != nil {
				t.Fatal(err)
			}
			if test.wantResult != 0 && result.kind != test.wantResult {
				t.Fatalf("result kind = %d, want %d", result.kind, test.wantResult)
			}
			diagnostic := capture.output.Diagnostic()
			if !strings.Contains(diagnostic, test.wantDiagnostic) {
				t.Fatalf("bootstrap diagnostic = %q", diagnostic)
			}
			if test.forbidDiagnostic != "" && strings.Contains(diagnostic, test.forbidDiagnostic) {
				t.Fatalf("bootstrap diagnostic retained post-READY bytes: %q", diagnostic)
			}
			if len(diagnostic) > verifierStderrMaxBytes+len(" [truncated]") {
				t.Fatalf("bootstrap diagnostic length = %d", len(diagnostic))
			}
		})
	}
}

func TestVerifierProtocolHelper(t *testing.T) {
	mode := os.Getenv("HELMR_VERIFIER_PROTOCOL_HELPER")
	if mode == "" {
		return
	}
	result := os.NewFile(uintptr(verifierResultFD), "verifier-result")
	if result == nil {
		os.Exit(90)
	}
	switch mode {
	case "ready-invalid":
		_, _ = fmt.Fprint(os.Stderr, "pre-ready diagnostic")
		if err := writeVerifierReady(result); err != nil {
			os.Exit(91)
		}
		time.Sleep(50 * time.Millisecond)
		signal.Ignore(syscall.SIGPIPE)
		_, _ = fmt.Fprint(os.Stderr, "post-ready tenant sentinel")
		if err := writeVerifierInvalid(result, runtimeVerifierJob.invalidDiagnostic()); err != nil {
			os.Exit(92)
		}
		os.Exit(0)
	case "graceful-pre-ready":
		_, _ = fmt.Fprint(os.Stderr, "graceful pre-ready failure")
		os.Exit(12)
	case "abort-pre-ready":
		_, _ = fmt.Fprint(os.Stderr, "abort pre-ready failure", strings.Repeat("x", verifierStderrMaxBytes*2))
		_ = syscall.Kill(os.Getpid(), syscall.SIGABRT)
		time.Sleep(time.Second)
		os.Exit(93)
	default:
		os.Exit(94)
	}
}

func TestExactVerifierChildQualificationWhenPrivileged(t *testing.T) {
	cgroupRoot := os.Getenv("HELMR_TEST_VERIFIER_CGROUP_ROOT")
	if cgroupRoot == "" || os.Geteuid() != 0 {
		t.Skip("requires root and HELMR_TEST_VERIFIER_CGROUP_ROOT")
	}
	if err := QualifyArtifactVerifier(t.Context(), cgroupRoot, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifierCommandUsesJobFDLayoutAndNamespaces(t *testing.T) {
	for _, test := range []struct {
		name  string
		job   verifierJob
		count int
	}{
		{name: "runtime", job: runtimeVerifierJob, count: 1},
		{name: "program", job: programVerifierJob, count: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := verifierTestFile(t, "result")
			artifacts := make([]*os.File, test.count)
			for index := range artifacts {
				artifacts[index] = verifierTestFile(t, "artifact")
			}
			pidFD := -1
			command := newVerifierCommand(
				context.Background(),
				verifierProcessConfig{job: test.job},
				17,
				&pidFD,
				result,
				artifacts,
			)
			if command.Path != verifierExecutable ||
				len(command.Args) != 3 ||
				command.Args[1] != verifierChildArgument ||
				command.Args[2] != string(test.job) {
				t.Fatalf("command = (%q, %q)", command.Path, command.Args)
			}
			if command.Env == nil || len(command.Env) != 0 {
				t.Fatalf("environment = %#v, want a non-nil empty environment", command.Env)
			}
			if command.Stdin != nil {
				t.Fatalf("stdin = %#v, want nil", command.Stdin)
			}
			if len(command.ExtraFiles) != test.count+1 ||
				command.ExtraFiles[verifierResultFD-3] != result {
				t.Fatalf("extra files = %#v", command.ExtraFiles)
			}
			for index, artifact := range artifacts {
				if command.ExtraFiles[verifierArtifactBaseFD-3+index] != artifact {
					t.Fatalf("artifact %d has the wrong child descriptor", index)
				}
			}
			if command.SysProcAttr == nil {
				t.Fatal("SysProcAttr is nil")
			}
			wantCloneNamespaces := uintptr(
				syscall.CLONE_NEWNET |
					syscall.CLONE_NEWPID |
					syscall.CLONE_NEWIPC,
			)
			if command.SysProcAttr.Cloneflags != wantCloneNamespaces {
				t.Fatalf(
					"clone flags = %#x, want %#x",
					command.SysProcAttr.Cloneflags,
					wantCloneNamespaces,
				)
			}
			if command.SysProcAttr.Unshareflags != syscall.CLONE_NEWNS {
				t.Fatalf(
					"unshare flags = %#x, want %#x",
					command.SysProcAttr.Unshareflags,
					syscall.CLONE_NEWNS,
				)
			}
			if !command.SysProcAttr.UseCgroupFD || command.SysProcAttr.CgroupFD != 17 {
				t.Fatalf(
					"cgroup placement = (%t, %d), want (true, 17)",
					command.SysProcAttr.UseCgroupFD,
					command.SysProcAttr.CgroupFD,
				)
			}
			if command.SysProcAttr.PidFD != &pidFD {
				t.Fatal("PidFD pointer was not installed")
			}
		})
	}
}

func TestVerifierProcessConfigRequiresClosedJobLayout(t *testing.T) {
	artifact := verifierTestFile(t, "artifact")
	valid := verifierProcessConfig{
		job:            runtimeVerifierJob,
		unitCgroupRoot: "/sys/fs/cgroup/helmr-worker.service",
		leaseIdentity:  "lease-123",
		artifacts:      []*os.File{artifact},
	}
	if err := validateVerifierProcessConfig(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*verifierProcessConfig){
		"invalid job": func(config *verifierProcessConfig) {
			config.job = "other"
		},
		"wrong count": func(config *verifierProcessConfig) {
			config.artifacts = append(config.artifacts, artifact)
		},
		"nil artifact": func(config *verifierProcessConfig) {
			config.artifacts[0] = nil
		},
		"relative cgroup": func(config *verifierProcessConfig) {
			config.unitCgroupRoot = "cgroup"
		},
		"unsafe lease": func(config *verifierProcessConfig) {
			config.leaseIdentity = "../lease"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := valid
			config.artifacts = append([]*os.File(nil), valid.artifacts...)
			mutate(&config)
			if err := validateVerifierProcessConfig(context.Background(), config); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
	//lint:ignore SA1012 nil is the contract violation under test
	if err := validateVerifierProcessConfig(nil, valid); err == nil {
		t.Fatal("nil context was accepted")
	}
}

func TestVerifierCgroupLimits(t *testing.T) {
	path := t.TempDir()
	for _, limit := range verifierCgroupLimits {
		if err := os.WriteFile(filepath.Join(path, limit.file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cgroup, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cgroup.Close()
	if err := configureVerifierCgroup(int(cgroup.Fd()), programVerifierJob); err != nil {
		t.Fatal(err)
	}
	for _, limit := range verifierCgroupLimits {
		raw, err := os.ReadFile(filepath.Join(path, limit.file))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != limit.value {
			t.Fatalf("%s = %q, want %q", limit.file, raw, limit.value)
		}
	}
}

func TestVerifierCgroupRejectsUnsafeLeaseIdentity(t *testing.T) {
	for _, identity := range []string{"", "..", "../lease", "lease/name", "lease_name"} {
		t.Run(identity, func(t *testing.T) {
			if _, err := verifierCgroupLeaf(programVerifierJob, identity); err == nil {
				t.Fatalf("lease identity %q was accepted", identity)
			}
		})
	}
	leaf, err := verifierCgroupLeaf(runtimeVerifierJob, "lease-123")
	if err != nil {
		t.Fatal(err)
	}
	if leaf != "verifier-runtime-lease-123" {
		t.Fatalf("leaf = %q", leaf)
	}
}

func TestCreateVerifierCgroupRejectsSymlinkRoot(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "cgroup")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := createVerifierCgroup(
		programVerifierJob,
		linkRoot,
		"lease-123",
	); err == nil {
		t.Fatal("symlinked unit cgroup root was accepted")
	}
}

func TestVerifierCgroupCleanupHelpers(t *testing.T) {
	path := t.TempDir()
	killPath := filepath.Join(path, "cgroup.kill")
	if err := os.WriteFile(killPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := killVerifierCgroup(path); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(killPath); err != nil {
		t.Fatal(err)
	} else if string(raw) != "1" {
		t.Fatalf("cgroup.kill = %q, want 1", raw)
	}

	eventsPath := filepath.Join(path, "cgroup.events")
	if err := os.WriteFile(eventsPath, []byte("populated 1\nfrozen 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		temporary := eventsPath + ".new"
		_ = os.WriteFile(temporary, []byte("populated 0\nfrozen 0\n"), 0o644)
		_ = os.Rename(temporary, eventsPath)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitVerifierCgroupEmpty(ctx, path); err != nil {
		t.Fatal(err)
	}
}

func TestOpenVerifierSnapshotRequiresReadOnlyIndependentDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot")
	if err := os.WriteFile(path, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.Seek(3, 0); err != nil {
		t.Fatal(err)
	}
	snapshot, err := openVerifierSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var got [3]byte
	if _, err := snapshot.Read(got[:]); err != nil {
		t.Fatal(err)
	}
	if string(got[:]) != "abc" {
		t.Fatalf("snapshot starts with %q, want abc", got)
	}
	if offset, err := source.Seek(0, 1); err != nil {
		t.Fatal(err)
	} else if offset != 3 {
		t.Fatalf("source offset = %d, want 3", offset)
	}

	writable, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	if _, err := openVerifierSnapshot(writable); err == nil {
		t.Fatal("writable Artifact descriptor was accepted")
	}
}

func TestVerifierReadinessDeadline(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if err := reader.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := readVerifierReady(reader); err == nil {
		t.Fatal("missing readiness did not time out")
	}
}

func TestVerifierTerminalDrainRejectsLeakedWriter(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	var output bytes.Buffer
	if err := writeVerifierVerified(
		&output,
		programVerifierJob,
		canonicalVerifierProgramVerification(t),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(output.Bytes()); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	wait <- nil
	_, resultErr, waitErr := awaitVerifierTerminal(
		context.Background(),
		reader,
		programVerifierJob,
		wait,
		20*time.Millisecond,
		func() {
			_ = reader.Close()
		},
	)
	if waitErr != nil {
		t.Fatalf("wait error = %v", waitErr)
	}
	if resultErr == nil || !strings.Contains(resultErr.Error(), "remained open") {
		t.Fatalf("result error = %v", resultErr)
	}
}

func verifierTestFile(t *testing.T, name string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		file.Close()
	})
	return file
}
