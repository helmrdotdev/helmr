package datapath

import "net/netip"

var managedCloudPublicIPv4DenyPrefixes = [9]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// ManagedCloudPublicIPv4DenyPrefixes returns the immutable destination set for
// the managed-Cloud public-IPv4 datapath profile.
func ManagedCloudPublicIPv4DenyPrefixes() [9]netip.Prefix {
	return managedCloudPublicIPv4DenyPrefixes
}
