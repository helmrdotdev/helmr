//go:build linux

package firecracker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"
)

func configuredNetworkPools(cfg Config) (netip.Prefix, netip.Prefix, error) {
	linkPool, err := netip.ParsePrefix(strings.TrimSpace(cfg.NetworkLinkPool))
	if err != nil || !linkPool.Addr().Is4() || linkPool != linkPool.Masked() || linkPool.Bits() > 31 {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network link pool must be canonical IPv4 CIDR with /31 capacity")
	}
	translationPool, err := netip.ParsePrefix(strings.TrimSpace(cfg.NetworkTranslationPool))
	if err != nil || !translationPool.Addr().Is4() || translationPool != translationPool.Masked() {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network translation pool must be canonical IPv4 CIDR")
	}
	if prefixesOverlap(linkPool, translationPool) {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network pools overlap")
	}
	guestPrefix := netip.MustParsePrefix(GuestNetworkCIDRV0).Masked()
	if prefixesOverlap(linkPool, guestPrefix) || prefixesOverlap(translationPool, guestPrefix) {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network pool overlaps the v0 guest subnet")
	}
	if cfg.NetworkCapacity <= 0 || uint64(cfg.NetworkCapacity) > prefixCapacity(linkPool)/2 || uint64(cfg.NetworkCapacity) > prefixCapacity(translationPool) {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network pools do not cover configured capacity")
	}
	resolver, err := netip.ParseAddr(strings.TrimSpace(cfg.NetworkResolverIPv4))
	if err != nil || !resolver.Is4() || resolver.IsUnspecified() {
		return netip.Prefix{}, netip.Prefix{}, errors.New("worker network resolver must be specified IPv4")
	}
	return linkPool, translationPool, nil
}

func validateNetworkPools(cfg Config) (netip.Prefix, netip.Prefix, error) {
	linkPool, translationPool, err := configuredNetworkPools(cfg)
	if err != nil {
		return netip.Prefix{}, netip.Prefix{}, err
	}
	if err := rejectLiveNetworkOverlap(linkPool, translationPool); err != nil {
		return netip.Prefix{}, netip.Prefix{}, err
	}
	return linkPool, translationPool, nil
}

func rejectLiveNetworkOverlap(pools ...netip.Prefix) error {
	addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("inventory worker IPv4 addresses: %w", err)
	}
	for _, address := range addresses {
		if address.IP == nil {
			continue
		}
		parsed, ok := netip.AddrFromSlice(address.IP)
		if !ok || !parsed.Unmap().Is4() {
			continue
		}
		for _, pool := range pools {
			if pool.Contains(parsed.Unmap()) {
				return fmt.Errorf("worker network pool %s overlaps live address %s", pool, parsed.Unmap())
			}
		}
	}
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("inventory worker IPv4 routes: %w", err)
	}
	for _, route := range routes {
		if route.Dst == nil {
			continue
		}
		routePrefix, ok := netipPrefix(route.Dst)
		if !ok {
			continue
		}
		for _, pool := range pools {
			if networkPoolRouteConflict(pool, routePrefix) {
				return fmt.Errorf("worker network pool %s overlaps live route %s", pool, routePrefix)
			}
		}
	}
	return nil
}

func networkPoolRouteConflict(pool, routePrefix netip.Prefix) bool {
	return routePrefix.Bits() > 0 && prefixesOverlap(pool, routePrefix)
}

func hostIPv4Prefixes(excluded ...netip.Prefix) ([]netip.Prefix, error) {
	addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("inventory worker IPv4 addresses: %w", err)
	}
	prefixes := make([]netip.Prefix, 0, len(addresses))
	for _, address := range addresses {
		parsed, ok := netip.AddrFromSlice(address.IP)
		if !ok || !parsed.Unmap().Is4() || parsed.IsUnspecified() {
			continue
		}
		parsed = parsed.Unmap()
		if slices.ContainsFunc(excluded, func(prefix netip.Prefix) bool { return prefix.Contains(parsed) }) {
			continue
		}
		prefixes = append(prefixes, netip.PrefixFrom(parsed, 32))
	}
	if len(prefixes) == 0 {
		return nil, errors.New("worker has no concrete IPv4 address")
	}
	return prefixes, nil
}

func prefixCapacity(prefix netip.Prefix) uint64 {
	return uint64(1) << uint(32-prefix.Bits())
}

func prefixAddress(prefix netip.Prefix, offset uint64) (netip.Addr, error) {
	if offset >= prefixCapacity(prefix) {
		return netip.Addr{}, fmt.Errorf("address offset %d exceeds pool %s", offset, prefix)
	}
	base := binary.BigEndian.Uint32(prefix.Addr().AsSlice())
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], base+uint32(offset))
	return netip.AddrFrom4(raw), nil
}

func prefixesOverlap(left, right netip.Prefix) bool {
	return left.Contains(right.Addr()) || right.Contains(left.Addr())
}

func netipPrefix(network *net.IPNet) (netip.Prefix, bool) {
	if network == nil {
		return netip.Prefix{}, false
	}
	address, ok := netip.AddrFromSlice(network.IP)
	ones, bits := network.Mask.Size()
	if !ok || !address.Unmap().Is4() || bits != 32 || ones < 0 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(address.Unmap(), ones).Masked(), true
}
