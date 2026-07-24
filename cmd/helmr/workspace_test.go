package main

import (
	"testing"
)

func TestWorkspaceAddressRequiresExactlyOneAddress(t *testing.T) {
	for _, test := range []struct {
		name    string
		address workspaceAddressFlags
		wantErr bool
	}{
		{name: "id", address: workspaceAddressFlags{id: "wsp_example"}},
		{name: "key", address: workspaceAddressFlags{key: "repository", declaredID: "repository-agent"}},
		{name: "missing", wantErr: true},
		{name: "both", address: workspaceAddressFlags{id: "wsp_example", key: "repository"}, wantErr: true},
		{name: "key without declaration", address: workspaceAddressFlags{key: "repository"}, wantErr: true},
		{name: "declaration with id", address: workspaceAddressFlags{id: "wsp_example", declaredID: "repository-agent"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.address.validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("validate() error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func TestWorkspaceSecretsPreserveDeclaredPlacements(t *testing.T) {
	secrets, err := workspaceSecrets(
		[]string{"github=GITHUB_TOKEN"},
		[]string{"config=/run/helmr-secrets/config.json"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 2 ||
		secrets[0].Name != "github" || secrets[0].Env != "GITHUB_TOKEN" ||
		secrets[1].Name != "config" || secrets[1].File != "/run/helmr-secrets/config.json" {
		t.Fatalf("secrets = %#v", secrets)
	}
}

func TestWorkspacePairSplitsOnlyFirstEquals(t *testing.T) {
	name, value, err := workspacePair("TOKEN=a=b", "--env")
	if err != nil {
		t.Fatal(err)
	}
	if name != "TOKEN" || value != "a=b" {
		t.Fatalf("pair = %q, %q", name, value)
	}
}
