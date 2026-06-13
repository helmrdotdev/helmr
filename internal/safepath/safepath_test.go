package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanSlash(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		options CleanOptions
		want    string
		wantErr bool
	}{
		{name: "relative", raw: "dir/file", want: "dir/file"},
		{name: "cleans", raw: "dir/./file", want: "dir/file"},
		{name: "dot allowed", raw: ".", options: CleanOptions{AllowDot: true}, want: "."},
		{name: "empty", raw: "", wantErr: true},
		{name: "dot rejected", raw: ".", wantErr: true},
		{name: "absolute", raw: "/tmp/file", wantErr: true},
		{name: "parent", raw: "../file", wantErr: true},
		{name: "nested parent", raw: "dir/../file", wantErr: true},
		{name: "clean escape", raw: "dir/../../file", wantErr: true},
		{name: "nul", raw: "dir/\x00/file", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CleanSlash(tt.raw, tt.options)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CleanSlash() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("CleanSlash() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("CleanSlash() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContains(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child", "file")
	inside, err := Contains(root, child)
	if err != nil {
		t.Fatalf("Contains() error = %v", err)
	}
	if !inside {
		t.Fatalf("Contains() = false, want true")
	}
	outside, err := Contains(root, filepath.Join(root, "..", "elsewhere"))
	if err != nil {
		t.Fatalf("Contains() escape error = %v", err)
	}
	if outside {
		t.Fatalf("Contains() escape = true, want false")
	}
}

func TestMkdirAllNoSymlinkRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err := MkdirAllNoSymlink(root, "link/child", 0o755)
	if err == nil {
		t.Fatalf("MkdirAllNoSymlink() error = nil")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "child")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("outside child stat error = %v, want not exist", statErr)
	}
}
