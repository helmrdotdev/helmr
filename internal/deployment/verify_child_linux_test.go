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

func TestParseProgramVerifierIdentity(t *testing.T) {
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

	uid, primaryGID, err := parseProgramVerifierPasswd(passwd)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := parseProgramVerifierGroup(group)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 992 || primaryGID != 991 || gid != 991 {
		t.Fatalf("identity = (%d, %d, %d)", uid, primaryGID, gid)
	}
}

func TestParseProgramVerifierIdentityRejectsMissingDuplicateAndRoot(t *testing.T) {
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
			if _, _, err := parseProgramVerifierPasswd(test.passwd); err == nil {
				t.Fatal("passwd entry was accepted")
			}
			if _, err := parseProgramVerifierGroup(test.group); err == nil {
				t.Fatal("group entry was accepted")
			}
		})
	}
}

func TestDigestProgramVerifierDescriptorUsesExactBytes(t *testing.T) {
	raw := bytes.Repeat([]byte("artifact"), 32769)
	digest, err := digestProgramVerifierDescriptor(
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

	_, err = programArtifactFromDescriptor(
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

func TestClassifyProgramVerifierError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		kind       programVerifierRecordKind
		diagnostic string
	}{
		{
			name:       "content",
			err:        &artifactContentError{cause: errors.New("invalid image")},
			kind:       programVerifierInvalid,
			diagnostic: "program is invalid",
		},
		{
			name:       "infrastructure",
			err:        &artifactInfrastructureError{cause: io.ErrUnexpectedEOF},
			kind:       programVerifierFailed,
			diagnostic: "program verifier infrastructure failure",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, diagnostic := classifyProgramVerifierError(test.err)
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
