//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"strings"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"golang.org/x/sys/unix"
)

func (c *Connector) withTapOwner() firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(firecracker.SetupNetworkHandlerName, firecracker.Handler{
			Name: "helmr.SetTapOwner",
			Fn: func(ctx context.Context, machine *firecracker.Machine) error {
				for _, iface := range machine.Cfg.NetworkInterfaces {
					if iface.StaticConfiguration == nil || iface.StaticConfiguration.HostDevName == "" {
						continue
					}
					if err := setTapOwner(machine.Cfg.NetNS, iface.StaticConfiguration.HostDevName, c.cfg.JailerUID, c.cfg.JailerGID); err != nil {
						return err
					}
				}
				return nil
			},
		})
	}
}

func setTapOwner(netNSPath string, tapName string, uid int, gid int) error {
	if strings.TrimSpace(netNSPath) == "" {
		return setTapOwnerInCurrentNetNS(tapName, uid, gid)
	}
	netNS, err := ns.GetNS(netNSPath)
	if err != nil {
		return fmt.Errorf("open network namespace %q: %w", netNSPath, err)
	}
	defer netNS.Close()
	return netNS.Do(func(ns.NetNS) error {
		return setTapOwnerInCurrentNetNS(tapName, uid, gid)
	})
}

func setTapOwnerInCurrentNetNS(tapName string, uid int, gid int) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq(tapName)
	if err != nil {
		return fmt.Errorf("build tap ifreq %q: %w", tapName, err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return fmt.Errorf("open tap device %q: %w", tapName, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETOWNER, uid); err != nil {
		return fmt.Errorf("set tap %q owner uid %d: %w", tapName, uid, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETGROUP, gid); err != nil {
		return fmt.Errorf("set tap %q owner gid %d: %w", tapName, gid, err)
	}
	return nil
}
