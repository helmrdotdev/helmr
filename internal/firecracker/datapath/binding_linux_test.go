//go:build linux

package datapath

import (
	"net"
	"net/netip"
	"testing"
)

func TestDatapathAuthorityIsClosed(t *testing.T) {
	valid := Authority{WorkerEpoch: 2, OwnerID: "owner", Generation: 4}
	if err := validateAuthority(valid); err != nil {
		t.Fatal(err)
	}
	for _, authority := range []Authority{
		{},
		{WorkerEpoch: 2, OwnerID: " ", Generation: 4},
		{WorkerEpoch: 2, OwnerID: "owner"},
	} {
		if err := validateAuthority(authority); err == nil {
			t.Fatalf("invalid authority was accepted: %+v", authority)
		}
	}
}

func TestDatapathInterfaceFactsRequireExactUnicastIdentity(t *testing.T) {
	valid := InterfaceFacts{
		TapName:     "tap0",
		GuestIPv4:   netip.MustParseAddr("192.0.2.2"),
		GatewayIPv4: netip.MustParseAddr("192.0.2.1"),
		GuestMAC:    net.HardwareAddr{0x02, 0, 0, 0, 0, 1},
	}
	if err := validateInterfaceFacts(valid); err != nil {
		t.Fatal(err)
	}
	for _, facts := range []InterfaceFacts{
		{},
		func() InterfaceFacts { value := valid; value.TapName = " "; return value }(),
		func() InterfaceFacts { value := valid; value.GatewayIPv4 = value.GuestIPv4; return value }(),
		func() InterfaceFacts {
			value := valid
			value.GuestMAC = net.HardwareAddr{1, 0, 0, 0, 0, 1}
			return value
		}(),
	} {
		if err := validateInterfaceFacts(facts); err == nil {
			t.Fatalf("invalid interface facts were accepted: %+v", facts)
		}
	}
}

func TestDatapathManagerStartsUnverified(t *testing.T) {
	if err := NewManager().Health(); err == nil {
		t.Fatal("unverified datapath manager was healthy")
	}
}
