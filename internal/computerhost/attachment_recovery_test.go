package computerhost

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComputerAttachmentRecoveryPreservesOrdinaryFiles(t *testing.T) {
	root := t.TempDir()
	arena := filepath.Join(root, "computer-retained-1")
	if err := os.Mkdir(arena, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "ordinary"), filepath.Join(arena, "version")} {
		if err := os.WriteFile(path, []byte("retained"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkComputerAttachmentResidue(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "ordinary"), filepath.Join(arena, "version")} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "retained" {
			t.Fatalf("retained data changed: %q, %v", data, err)
		}
	}
}

func TestComputerAttachmentRecoveryRejectsCustodyAndAmbiguity(t *testing.T) {
	for _, kind := range []string{"config.json", "claim.json", "nbd.sock", "marker-symlink", "arena-symlink", "arena-file", "root-symlink", "missing-root"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			arena := filepath.Join(root, "computer-retained-1")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "arena-file":
				must(os.WriteFile(arena, nil, 0600))
			case "arena-symlink":
				must(os.Symlink(t.TempDir(), arena))
			case "root-symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				must(os.Symlink(root, alias))
				root = alias
			case "missing-root":
				root = filepath.Join(root, "missing")
			default:
				must(os.Mkdir(arena, 0700))
				if kind == "marker-symlink" {
					must(os.Symlink("absent", filepath.Join(arena, "claim.json")))
				} else {
					must(os.WriteFile(filepath.Join(arena, kind), []byte(`{"state":"released"}`), 0600))
				}
			}
			if err := checkComputerAttachmentResidue(root); err == nil {
				t.Fatal("accepted unresolved attachment custody")
			}
		})
	}
}

func TestComputerAttachmentRecoveryRejectsUnreadableDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires credentials that cannot bypass directory permissions")
	}
	for _, kind := range []string{"root", "arena"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			arena := filepath.Join(root, "computer-retained-1")
			if err := os.Mkdir(arena, 0700); err != nil {
				t.Fatal(err)
			}
			dataPath := filepath.Join(arena, "version")
			if err := os.WriteFile(dataPath, []byte("retained"), 0600); err != nil {
				t.Fatal(err)
			}
			blocked := arena
			if kind == "root" {
				blocked = root
			}
			if err := os.Chmod(blocked, 0); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(blocked, 0700)
			err := checkComputerAttachmentResidue(root)
			if restoreErr := os.Chmod(blocked, 0700); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			if err == nil {
				t.Fatal("unreadable custody accepted")
			}
			data, err := os.ReadFile(dataPath)
			if err != nil || string(data) != "retained" {
				t.Fatalf("rejection changed retained data: %q %v", data, err)
			}
		})
	}
}
