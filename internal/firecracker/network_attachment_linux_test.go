//go:build linux

package firecracker

import (
	"testing"
)

func TestStaticNetworkInterfaceMatchesVMRuntimeContract(t *testing.T) {
	iface := staticNetworkInterface()
	if iface.StaticConfiguration == nil {
		t.Fatalf("interface = %+v", iface)
	}
	static := iface.StaticConfiguration
	if static.HostDevName != GuestTapNameV0 || static.MacAddress != GuestMACV0 || static.IPConfiguration != nil {
		t.Fatalf("static interface = %+v", static)
	}
}
