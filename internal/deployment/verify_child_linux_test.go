//go:build linux

package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const verifierDescriptorIsolationHelper = "HELMR_TEST_VERIFIER_DESCRIPTOR_ISOLATION"
const verifierLauncherIsolationHelper = "HELMR_TEST_VERIFIER_LAUNCHER_ISOLATION"

func TestVerifierDescriptorIsolationPreservesGoRuntimeDescriptors(t *testing.T) {
	if os.Getenv(verifierDescriptorIsolationHelper) == "1" {
		result := os.NewFile(verifierResultFD, "verifier-result")
		if result == nil {
			os.Exit(20)
		}
		if err := isolateVerifierContractDescriptors(runtimeVerifierJob); err != nil {
			os.Exit(21)
		}
		resultTarget, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", verifierResultFD))
		if err != nil {
			os.Exit(26)
		}
		artifactTarget, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", verifierArtifactBaseFD))
		if err != nil {
			os.Exit(27)
		}
		child := exec.Command("/bin/sh", "-c", `
for descriptor in 3 4; do
  target="$(readlink "/proc/self/fd/$descriptor" 2>/dev/null || true)"
  [ "$target" != "$HELMR_TEST_RESULT_TARGET" ] || exit 1
  [ "$target" != "$HELMR_TEST_ARTIFACT_TARGET" ] || exit 1
done
`)
		child.Env = []string{
			"HELMR_TEST_ARTIFACT_TARGET=" + artifactTarget,
			"HELMR_TEST_RESULT_TARGET=" + resultTarget,
			"PATH=/usr/bin:/bin",
		}
		child.Stdin = strings.NewReader("")
		if err := child.Run(); err != nil {
			os.Exit(28)
		}
		// Sleeping makes the scheduler enter netpoll. The verifier must retain
		// the epoll descriptor that the Go runtime created after exec.
		time.Sleep(20 * time.Millisecond)
		reader, writer, err := os.Pipe()
		if err != nil {
			os.Exit(22)
		}
		defer reader.Close()
		defer writer.Close()
		if err := reader.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
			os.Exit(23)
		}
		var one [1]byte
		if _, err := reader.Read(one[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
			os.Exit(24)
		}
		if _, err := result.Write([]byte{1}); err != nil {
			os.Exit(25)
		}
		os.Exit(0)
	}

	resultReader, resultWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer resultReader.Close()
	artifact := verifierTestFile(t, "artifact")
	command := exec.Command(
		os.Args[0],
		"-test.run=^TestVerifierDescriptorIsolationPreservesGoRuntimeDescriptors$",
	)
	command.Env = append(os.Environ(), verifierDescriptorIsolationHelper+"=1")
	command.ExtraFiles = []*os.File{resultWriter, artifact}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := resultWriter.Close(); err != nil {
		t.Fatal(err)
	}
	var result [1]byte
	_, readErr := io.ReadFull(resultReader, result[:])
	waitErr := command.Wait()
	if readErr != nil || waitErr != nil || result[0] != 1 {
		t.Fatalf("descriptor isolation = (%v, %v, %d)", readErr, waitErr, result[0])
	}
}

func TestVerifierLauncherClosesUnlistedDescriptorOnExec(t *testing.T) {
	switch os.Getenv(verifierLauncherIsolationHelper) {
	case "launch":
		if err := unix.Dup3(verifierArtifactBaseFD, 100, 0); err != nil {
			os.Exit(30)
		}
		if err := markVerifierAmbientDescriptorsCloseOnExec(runtimeVerifierJob); err != nil {
			os.Exit(31)
		}
		if err := unix.Exec(
			os.Args[0],
			[]string{
				os.Args[0],
				"-test.run=^TestVerifierLauncherClosesUnlistedDescriptorOnExec$",
			},
			[]string{verifierLauncherIsolationHelper + "=verify"},
		); err != nil {
			os.Exit(32)
		}
	case "verify":
		for fd := verifierResultFD; fd <= verifierArtifactBaseFD; fd++ {
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
				os.Exit(33)
			}
		}
		if _, err := unix.FcntlInt(100, unix.F_GETFD, 0); !errors.Is(err, syscall.EBADF) {
			os.Exit(34)
		}
		result := os.NewFile(verifierResultFD, "verifier-result")
		if result == nil {
			os.Exit(35)
		}
		if _, err := result.Write([]byte{1}); err != nil {
			os.Exit(36)
		}
		os.Exit(0)
	}

	resultReader, resultWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer resultReader.Close()
	artifact := verifierTestFile(t, "artifact")
	command := exec.Command(
		os.Args[0],
		"-test.run=^TestVerifierLauncherClosesUnlistedDescriptorOnExec$",
	)
	command.Env = append(os.Environ(), verifierLauncherIsolationHelper+"=launch")
	command.ExtraFiles = []*os.File{resultWriter, artifact}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := resultWriter.Close(); err != nil {
		t.Fatal(err)
	}
	var result [1]byte
	_, readErr := io.ReadFull(resultReader, result[:])
	waitErr := command.Wait()
	if readErr != nil || waitErr != nil || result[0] != 1 {
		t.Fatalf("launcher isolation = (%v, %v, %d)", readErr, waitErr, result[0])
	}
}

func TestConformanceCommandDoesNotDependOnDevNull(t *testing.T) {
	command := newConformanceCommand(
		context.Background(),
		[]string{"PATH=/bin"},
		"/",
		io.Discard,
		io.Discard,
		"/bin/true",
	)
	if command.Stdin == nil {
		t.Fatal("conformance command would open /dev/null")
	}
	var one [1]byte
	if n, err := command.Stdin.Read(one[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("conformance command stdin = (%d, %v), want (0, EOF)", n, err)
	}
}

func TestParseVerifierIdentity(t *testing.T) {
	passwd := []byte(strings.Join([]string{
		"root:x:0:0:root:/root:/bin/sh",
		"helmr-verifier:x:992:991::/nonexistent:/usr/sbin/nologin",
		"",
	}, "\n"))
	group := []byte(strings.Join([]string{
		"root:x:0:",
		"helmr-verifier:x:991:",
		"",
	}, "\n"))

	uid, primaryGID, err := parseVerifierPasswd(passwd)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := parseVerifierGroup(group)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 992 || primaryGID != 991 || gid != 991 {
		t.Fatalf("identity = (%d, %d, %d)", uid, primaryGID, gid)
	}
}

func TestParseVerifierIdentityRejectsMissingDuplicateAndRoot(t *testing.T) {
	tests := map[string]struct {
		passwd []byte
		group  []byte
	}{
		"missing": {
			passwd: []byte("root:x:0:0:root:/root:/bin/sh\n"),
			group:  []byte("root:x:0:\n"),
		},
		"duplicate": {
			passwd: []byte(
				"helmr-verifier:x:992:991::/nonexistent:/bin/false\n" +
					"helmr-verifier:x:993:991::/nonexistent:/bin/false\n",
			),
			group: []byte("helmr-verifier:x:991:\nhelmr-verifier:x:991:\n"),
		},
		"root": {
			passwd: []byte("helmr-verifier:x:0:991::/nonexistent:/bin/false\n"),
			group:  []byte("helmr-verifier:x:0:\n"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseVerifierPasswd(test.passwd); err == nil {
				t.Fatal("passwd entry was accepted")
			}
			if _, err := parseVerifierGroup(test.group); err == nil {
				t.Fatal("group entry was accepted")
			}
		})
	}
}

func TestVerifierArtifactDescriptorRequiresWriteProtectedNonOwnerInode(t *testing.T) {
	path := t.TempDir() + "/artifact"
	if err := os.WriteFile(path, []byte("artifact"), 0o400); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	owner := uint32(os.Geteuid())
	if err := validateVerifierArtifactDescriptor(int(file.Fd()), &owner); err == nil ||
		!strings.Contains(err.Error(), "owned by the verifier") {
		t.Fatalf("owner validation error = %v", err)
	}
	nonOwner := owner ^ 1
	if err := validateVerifierArtifactDescriptor(int(file.Fd()), &nonOwner); err != nil {
		t.Fatalf("non-owner validation = %v", err)
	}

	if err := file.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateVerifierArtifactDescriptor(int(file.Fd()), &nonOwner); err == nil ||
		!strings.Contains(err.Error(), "write permission") {
		t.Fatalf("write permission validation error = %v", err)
	}
}

func TestDigestVerifierDescriptorUsesExactBytes(t *testing.T) {
	raw := bytes.Repeat([]byte("artifact"), 32769)
	digest, err := digestVerifierDescriptor(
		context.Background(),
		bytes.NewReader(raw),
		int64(len(raw)),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(raw))
	if digest != want {
		t.Fatalf("digest = %q, want %q", digest, want)
	}
}

func TestProgramArtifactFromDescriptorRejectsPhysicalBoundBeforeRead(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "artifact")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(maxProgramPhysicalBytes + 1); err != nil {
		t.Fatal(err)
	}

	_, err = artifactFromDescriptor(
		context.Background(),
		int(file.Fd()),
		programArtifact,
		ProgramArtifactMediaType,
	)
	var contentError *artifactContentError
	if !errors.As(err, &contentError) {
		t.Fatalf("error = %T %v, want content error", err, err)
	}
}

func TestClassifyVerifierError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		kind       verifierRecordKind
		diagnostic string
	}{
		{
			name:       "content",
			err:        &artifactContentError{cause: errors.New("invalid image")},
			kind:       verifierInvalid,
			diagnostic: "program is invalid",
		},
		{
			name:       "infrastructure",
			err:        &artifactInfrastructureError{cause: io.ErrUnexpectedEOF},
			kind:       verifierFailed,
			diagnostic: "program verifier infrastructure failure",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, diagnostic := classifyVerifierError(programVerifierJob, test.err)
			if kind != test.kind || diagnostic != test.diagnostic {
				t.Fatalf(
					"classification = (%d, %q), want (%d, %q)",
					kind,
					diagnostic,
					test.kind,
					test.diagnostic,
				)
			}
		})
	}
}
