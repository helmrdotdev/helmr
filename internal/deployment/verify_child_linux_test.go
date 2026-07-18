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
	"strings"
	"testing"
)

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
	if err := file.Truncate(maxCodePhysicalBytes + 1); err != nil {
		t.Fatal(err)
	}

	_, err = artifactFromDescriptor(
		context.Background(),
		int(file.Fd()),
		codeArtifact,
		ProgramCodeArtifactMediaType,
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
