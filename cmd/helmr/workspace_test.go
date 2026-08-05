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
		{name: "id", address: workspaceAddressFlags{id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}},
		{name: "key", address: workspaceAddressFlags{key: "repository"}},
		{name: "missing", wantErr: true},
		{name: "both", address: workspaceAddressFlags{id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", key: "repository"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.address.validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("validate() error = %v, wantErr = %v", err, test.wantErr)
			}
		})
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
