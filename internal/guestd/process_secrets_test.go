package guestd

import (
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"os"
	"path/filepath"
	"testing"
)

func TestProcessSecretStagingKeepsPeerMaterial(t *testing.T) {
	root := t.TempDir()
	stage := func(value string) (string, func()) {
		env := []string{}
		path, cleanup, err := stageProgramSecrets(root, []*agentv1.SessionSecret{{Placement: &agentv1.SessionSecret_File{File: "/secrets/token"}, Value: []byte(value)}}, nil, &env)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		return path, cleanup
	}
	first, cleanup := stage("first")
	second, _ := stage("second")
	if first == second {
		t.Fatal("shared Secret staging")
	}
	for path, want := range map[string]string{first: "first", second: "second"} {
		body, err := os.ReadFile(filepath.Join(path, "secrets/token"))
		if err != nil || string(body) != want {
			t.Fatalf("staged material=%q %v", body, err)
		}
	}
	cleanup()
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("first staging retained: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(second, "secrets/token"))
	if err != nil || string(body) != "second" {
		t.Fatalf("peer deleted: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets/token")); err != nil {
		t.Fatalf("peer mount target deleted: %v", err)
	}
}

func TestTrustPublicationPreservesOpenReader(t *testing.T) {
	target := filepath.Join(t.TempDir(), "ca.pem")
	if err := publishSecretTrustFile(target, []byte("old")); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := publishSecretTrustFile(target, []byte("new")); err != nil {
		t.Fatal(err)
	}
	old := make([]byte, 3)
	if _, err := reader.Read(old); err != nil || string(old) != "old" {
		t.Fatalf("open trust changed=%q %v", old, err)
	}
	current, err := os.ReadFile(target)
	if err != nil || string(current) != "new" {
		t.Fatalf("published trust=%q %v", current, err)
	}
}
