package main

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// configureNet6 puts the pod's routed IPv6 address on eth1 (the vmnet
// interface). The /64 is on-link; no routes needed here.
func configureNet6(ns *net6Spec) error {
	eth1, err := netlink.LinkByName("eth1")
	if err != nil {
		return fmt.Errorf("no eth1: %w", err)
	}
	addr, err := netlink.ParseAddr(ns.IP)
	if err != nil {
		return fmt.Errorf("parsing ip %q: %w", ns.IP, err)
	}
	// NODAD: a tentative (DAD-in-progress) address may not be used as a
	// source, so for the pod's first ~2s the kernel picked ::1 for outbound
	// flows — poisoning any TCP connection made at boot for its entire
	// life (retransmits keep the source). The ULA is derived from the pod
	// UID on a private bridge; duplicate detection buys nothing.
	addr.Flags = unix.IFA_F_NODAD
	if err := netlink.AddrAdd(eth1, addr); err != nil {
		return fmt.Errorf("adding address: %w", err)
	}
	if err := netlink.LinkSetUp(eth1); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	return nil
}

// configureNet brings up lo and eth0 with a static address via netlink —
// images can't be assumed to ship iproute2, so we do it ourselves.
func configureNet(ns *netSpec) error {
	if lo, err := netlink.LinkByName("lo"); err == nil {
		netlink.LinkSetUp(lo)
	}
	eth0, err := netlink.LinkByName("eth0")
	if err != nil {
		return fmt.Errorf("no eth0: %w", err)
	}
	addr, err := netlink.ParseAddr(ns.IP)
	if err != nil {
		return fmt.Errorf("parsing ip %q: %w", ns.IP, err)
	}
	if err := netlink.AddrAdd(eth0, addr); err != nil {
		return fmt.Errorf("adding address: %w", err)
	}
	if err := netlink.LinkSetUp(eth0); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	gw := net.ParseIP(ns.GW)
	if gw == nil {
		return fmt.Errorf("bad gateway %q", ns.GW)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: eth0.Attrs().Index,
		Gw:        gw,
	}); err != nil {
		return fmt.Errorf("adding default route: %w", err)
	}
	return nil
}
