package guestd

import (
	"os"
	"strings"
	"testing"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
)

func TestValidateProgramSecretsRequiresCanonicalNonConflictingPlacements(t *testing.T) {
	valid := []*agentv1.SessionSecret{
		{
			Placement: &agentv1.SessionSecret_Env{Env: "API_TOKEN"},
			Value:     []byte("value"),
		},
		{
			Placement: &agentv1.SessionSecret_File{
				File: "/run/helmr-secrets/config.json",
			},
			Value: []byte("{}"),
		},
	}
	if err := validateProgramSecrets(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		secrets []*agentv1.SessionSecret
	}{
		{
			name: "noncanonical",
			secrets: []*agentv1.SessionSecret{
				valid[1],
				valid[0],
			},
		},
		{
			name: "duplicate env",
			secrets: []*agentv1.SessionSecret{
				valid[0],
				{
					Placement: &agentv1.SessionSecret_Env{Env: "API_TOKEN"},
					Value:     []byte("other"),
				},
			},
		},
		{
			name: "durable file",
			secrets: []*agentv1.SessionSecret{
				{
					Placement: &agentv1.SessionSecret_File{
						File: "/workspace/token",
					},
					Value: []byte("value"),
				},
			},
		},
		{
			name: "nul value",
			secrets: []*agentv1.SessionSecret{
				{
					Placement: &agentv1.SessionSecret_Env{Env: "TOKEN"},
					Value:     []byte("sensitive\x00value"),
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateProgramSecrets(test.secrets)
			if err == nil {
				t.Fatal("validateProgramSecrets() error = nil")
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("Secret value leaked in error: %v", err)
			}
		})
	}
}

func TestPrepareProgramSecretTargetCleansOnlyCreatedImagePaths(t *testing.T) {
	imageRoot := t.TempDir()
	cleanup, err := prepareProgramSecretTarget(
		imageRoot,
		"/run/helmr-secrets/token",
	)
	if err != nil {
		t.Fatal(err)
	}
	target := imageRoot + "/run/helmr-secrets/token"
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("created target survived cleanup: %v", err)
	}

	existing := imageRoot + "/etc/config"
	if err := os.MkdirAll(imageRoot+"/etc", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanup, err = prepareProgramSecretTarget(imageRoot, "/etc/config")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	body, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "image" {
		t.Fatalf("existing target body = %q", body)
	}
}

func TestPrepareProgramSecretTargetRejectsSymlinkParent(t *testing.T) {
	imageRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, imageRoot+"/etc"); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareProgramSecretTarget(
		imageRoot,
		"/etc/token",
	); err == nil {
		t.Fatal("symlink parent was accepted")
	}
	if _, err := os.Stat(outside + "/token"); !os.IsNotExist(err) {
		t.Fatalf("Secret target escaped image root: %v", err)
	}
}
