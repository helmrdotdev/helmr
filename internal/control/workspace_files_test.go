package control

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestValidateWorkspaceFilePathRequiresCanonicalRootRelativeUTF8(t *testing.T) {
	for _, value := range []string{".", "src/main.ts", "name with spaces.txt"} {
		if got, err := validateWorkspaceFilePath(value); err != nil || got != value {
			t.Fatalf("validateWorkspaceFilePath(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"", "/etc/passwd", "../secret", "src/../secret", "src//main.ts", "src\x00main"} {
		if _, err := validateWorkspaceFilePath(value); err == nil {
			t.Fatalf("validateWorkspaceFilePath(%q) succeeded", value)
		}
	}
}

func TestWorkspaceFileCursorPinsWorkspaceVersionAndPath(t *testing.T) {
	server := &Server{authKeys: auth.Keys{WorkspaceFileCursor: make([]byte, auth.RootKeySize)}}
	now := time.Unix(1_800_000_000, 0)
	cursor := workspaceFileCursor{
		WorkspaceID: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		VersionID:   "wsv_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		Path:        "src",
		After:       "src/main.ts",
		ExpiresAt:   now.Add(workspaceFileCursorTTL).Unix(),
	}
	token, err := server.signWorkspaceFileCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := server.parseWorkspaceFileCursor(token, cursor.WorkspaceID, cursor.Path, now)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != cursor {
		t.Fatalf("cursor = %#v", parsed)
	}
	if _, err := server.parseWorkspaceFileCursor(token+"x", cursor.WorkspaceID, cursor.Path, now); err == nil {
		t.Fatal("tampered cursor succeeded")
	}
	if _, err := server.parseWorkspaceFileCursor(token, cursor.WorkspaceID, ".", now); err == nil {
		t.Fatal("cursor retargeted to another path")
	}
	if _, err := server.parseWorkspaceFileCursor(token, cursor.WorkspaceID, cursor.Path, now.Add(workspaceFileCursorTTL)); !errors.Is(err, errWorkspaceFileCursorExpired) {
		t.Fatalf("expired cursor error = %v", err)
	}
}

func TestReadWorkspaceFileSourceUsesResolvedDigest(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	content := []byte("hello")
	if err := writer.WriteHeader(&tar.Header{
		Name: "src/main.txt",
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	store := &workspaceFileCAS{body: body.Bytes()}
	server := &Server{cas: store}
	result, err := server.readWorkspaceFileSource(context.Background(), workspaceFileSource{
		digest: "sha256:resolved",
	}, "src/main.txt")
	if err != nil {
		t.Fatal(err)
	}
	if store.digest != "sha256:resolved" {
		t.Fatalf("CAS digest = %q", store.digest)
	}
	if result.DataBase64 != "aGVsbG8=" {
		t.Fatalf("file content = %q", result.DataBase64)
	}
}

type workspaceFileCAS struct {
	digest string
	body   []byte
}

func (*workspaceFileCAS) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected CAS stat")
}

func (store *workspaceFileCAS) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	store.digest = digest
	return io.NopCloser(bytes.NewReader(store.body)), nil
}

func (*workspaceFileCAS) Put(context.Context, string, io.Reader) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected CAS put")
}

func (*workspaceFileCAS) Stage(context.Context, string) (cas.Stage, error) {
	return nil, errors.New("unexpected CAS stage")
}

func (*workspaceFileCAS) Delete(context.Context, string) error {
	return errors.New("unexpected CAS delete")
}
