package networkpolicy

import (
	"encoding/json"
	"net/netip"
	"testing"
)

func TestPolicyGrants(t *testing.T) {
	p, err := ParsePublic(json.RawMessage(`{"allowDomains":["*.example.com",{"domain":"db.corp.example","ipRanges":["10.60.0.0/16"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	in := Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.0.0/16")}, BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("10.90.0.0/16")}}
	if err := p.ValidateInstallation(in); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host, ip string
		allow    bool
	}{{"example.com", "8.8.8.8", false}, {"a.b.example.com", "8.8.8.8", true}, {"db.corp.example", "10.60.1.2", true}, {"a.example.com", "10.60.1.2", false}, {"db.corp.example", "8.8.8.8", false}, {"a.example.com", "169.254.169.254", false}, {"db.corp.example", "10.61.1.2", false}} {
		if got := p.AllowsAddress(c.host, netip.MustParseAddr(c.ip), in); got != c.allow {
			t.Fatalf("%s %s = %t", c.host, c.ip, got)
		}
	}
	raw, _ := json.Marshal(p)
	decoded, err := DecodeStored(raw)
	if err != nil || decoded.Digest() != p.Digest() {
		t.Fatalf("stored roundtrip %s %v", raw, err)
	}
}
func TestRejectMalformed(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `true`, `{"blockAll":true,"allowDomains":[]}`, `{"blockAll":null}`, `{"allowDomains":null}`, `{"allowDomains":["*.com"]}`, `{"allowDomains":["*.example.com","*.EXAMPLE.com"]}`, `{"allowDomains":[{"domain":"a.example.com","ipRanges":[]}]}`, `{"allowDomains":[{"domain":"a.example.com","ipRanges":["10.0.0.1/8"]}]}`, `{"allowDomains":["127.0.0.1"]}`, `{"allowDomains":["a.example.com.."]}`} {
		if _, err := ParsePublic(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	p, err := ParsePublic(nil)
	if err != nil || p.Kind != Ordinary {
		t.Fatal(p, err)
	}
	p, err = ParsePublic(json.RawMessage(`{"allowDomains":[]}`))
	if err != nil || p.Kind != Blocked {
		t.Fatal(p, err)
	}
	if _, err := DecodeStored(nil); err == nil {
		t.Fatal("missing authority accepted")
	}
}
func TestInstallationSemanticCoverage(t *testing.T) {
	control := Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.0.0/16")}, BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("10.90.0.0/16")}}
	host := Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.0.0/17")}, BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("10.90.0.0/17"), netip.MustParsePrefix("10.90.128.0/17")}}
	if !Compatible(host, control) || Compatible(control, host) {
		t.Fatal("semantic containment failed")
	}
	if OrdinaryPolicy().AllowsAddress("", netip.MustParseAddr("10.61.1.2"), control) {
		t.Fatal("ordinary widened private ceiling")
	}
	host.PrivateBusinessCIDRs = append(host.PrivateBusinessCIDRs, netip.MustParsePrefix("10.90.0.0/24"))
	if ValidateInstallation(host) == nil {
		t.Fatal("management overlap accepted")
	}
}

func TestJSONDuplicateMembersAndUnicode(t *testing.T) {
	for _, raw := range []string{`{"blockAll":true,"blockAll":false}`, `{"allowDomains":[{"domain":"a.example.com","domain":"b.example.com","ipRanges":["8.8.8.0/24"]}]}`, `{"allowDomains":["K.example.com"]}`} {
		if _, err := ParsePublic(json.RawMessage(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	if _, err := DecodeStored(json.RawMessage(`{"kind":"ordinary","kind":"block"}`)); err == nil {
		t.Fatal("duplicate stored authority")
	}
}

func TestTrustedPolicyAndInstallationDecoding(t *testing.T) {
	for _, raw := range []string{`{"kind":"ordinary","domains":[]}`, `{"kind":"allow","domains":[{"domain":"API.example.com"}]}`, `{"kind":"ordinary","futureRestriction":true}`, `null`} {
		var p Policy
		if json.Unmarshal([]byte(raw), &p) == nil {
			t.Fatal("accepted noncanonical transport", raw)
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"privateBusinessCidrs":null,"blockedIpv4Cidrs":[]}`, `{"privateBusinessCidrs":[],"blockedIpv4Cidrs":[],"other":[]}`, `{"privateBusinessCidrs":["8.8.0.0/16"],"blockedIpv4Cidrs":[]}`} {
		var in Installation
		if json.Unmarshal([]byte(raw), &in) == nil {
			t.Fatal("accepted missing/invalid installation", raw)
		}
	}
	raw, err := json.Marshal(Installation{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded Installation
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(string(raw), err)
	}
}
