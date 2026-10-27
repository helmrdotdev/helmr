package deployment

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"net/netip"
	"testing"
)

func TestPublicationChecksBothPhasePoliciesAgainstInstallation(t *testing.T) {
	metadata := artifacttest.ProgramMetadata(t)
	m := metadata.Definitions[2].Template
	m.Prepare = true
	m.Network = []byte(`{"allowDomains":[{"domain":"api.corp.example","ipRanges":["10.60.0.0/16"]}]}`)
	m.PrepareNetwork = []byte(`{"allowDomains":[{"domain":"packages.corp.example","ipRanges":["10.40.0.0/16"]}]}`)
	installation := networkpolicy.Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.0.0/16")}}
	if err := validatePublicationNetwork(metadata, installation); !errors.Is(err, definition.ErrInvalidNetworkPolicy) {
		t.Fatalf("preparation outside ceiling: %v", err)
	}
	installation.PrivateBusinessCIDRs = append(installation.PrivateBusinessCIDRs, netip.MustParsePrefix("10.40.0.0/16"))
	if err := validatePublicationNetwork(metadata, installation); err != nil {
		t.Fatal(err)
	}
	installation.BlockedIPv4CIDRs = []netip.Prefix{netip.MustParsePrefix("1.1.1.0/24")}
	m.Network = []byte(`{"allowDomains":[{"domain":"api.example","ipRanges":["1.1.1.0/24"]}]}`)
	if err := validatePublicationNetwork(metadata, installation); !errors.Is(err, definition.ErrInvalidNetworkPolicy) {
		t.Fatalf("mandatory deny: %v", err)
	}
}
func TestDefinitionReadRejectsMissingOrMalformedAuthority(t *testing.T) {
	for _, item := range []DefinitionRecord{{}, {Network: []byte(`null`), PrepareNetwork: []byte(`{"kind":"block"}`)}, {Network: []byte(`{"kind":"ordinary"}`), PrepareNetwork: []byte(`{"kind":"other"}`)}} {
		if err := projectDefinitionPolicies(&item, definition.KindTemplate); err == nil {
			t.Fatal("trusted policy omission accepted")
		}
	}
	for _, raw := range [][]byte{nil, []byte(`null`), []byte(`{"kind":"allow","rules":null}`)} {
		item := DefinitionRecord{RuntimeMcp: raw}
		if err := projectDefinitionPolicies(&item, definition.KindAgent); err == nil {
			t.Fatal("trusted MCP omission accepted")
		}
	}
}
