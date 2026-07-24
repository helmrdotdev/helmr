package control

import (
	"errors"
	"testing"
	"time"
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
	server := &Server{authSecret: []byte("01234567890123456789012345678901")}
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
