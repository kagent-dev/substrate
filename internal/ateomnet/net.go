//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ateomnet provides shared networking configuration logic for Substrate runtime agents.
package ateomnet

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	HostVethName     = "ateom0"
	ActorVethName    = "eth0"
	HostVethCIDR     = "169.254.17.1/30"
	ActorVethCIDR    = "169.254.17.2/30"
	ActorVethGateway = "169.254.17.1"
	ActorVethIP      = "169.254.17.2"

	// ActorVethSubnet is the point-to-point /30 the actor veth lives on.
	ActorVethSubnet = "169.254.17.0/30"
)

var (
	HostVethAddr  = MustParseAddr(HostVethCIDR)
	ActorVethAddr = MustParseAddr(ActorVethCIDR)
	ActorVethGwIP = MustParseIP(ActorVethGateway)
)

// MustParseAddr parses a CIDR string into a netlink.Addr, panicking on error.
func MustParseAddr(cidr string) *netlink.Addr {
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		panic(fmt.Sprintf("parsing constant CIDR %q: %v", cidr, err))
	}
	return a
}

// MustParseIP parses an IPv4 string into a net.IP, panicking on error.
func MustParseIP(s string) net.IP {
	ip := net.ParseIP(s).To4()
	if ip == nil {
		panic(fmt.Sprintf("parsing constant IPv4 %q", s))
	}
	return ip
}

// MustParseMAC parses a MAC address string into a net.HardwareAddr, panicking on error.
func MustParseMAC(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(fmt.Sprintf("parsing constant MAC %q: %v", s, err))
	}
	return m
}

// ConfigureActorVeth configures the actor veth inside the interior netns.
// It assumes it is already running inside the target network namespace.
func ConfigureActorVeth(ctx context.Context) error {
	// Run inside the gVisor interior netns. Sandbox.Setup has already created
	// the veth peer here, under its final name, so this only has to address it.
	// gVisor reads link names, addresses, and routes from this namespace when the
	// workload starts, so eth0 is configured like a normal container interface:
	//
	//   * lo is brought up for localhost behavior.
	//   * eth0 receives the actor-side /30 address.
	//   * the default route points to the gateway namespace's veth.
	//
	// This route delivers arbitrary destinations to capture. Gateway itself has
	// no default route and drops forwarding; the runtime route is not an uplink.
	loLink, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("while acquiring lo in interior netns: %w", err)
	}
	if err := netlink.LinkSetUp(loLink); err != nil {
		return fmt.Errorf("while bringing up lo in interior netns: %w", err)
	}

	actorLink, err := netlink.LinkByName(ActorVethName)
	if err != nil {
		return fmt.Errorf("while acquiring actor veth in interior netns: %w", err)
	}

	if err := netlink.AddrReplace(actorLink, ActorVethAddr); err != nil {
		return fmt.Errorf("while assigning actor veth address: %w", err)
	}
	if err := netlink.LinkSetUp(actorLink); err != nil {
		return fmt.Errorf("while bringing up actor veth: %w", err)
	}

	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: actorLink.Attrs().Index,
		Gw:        ActorVethGwIP,
	}); err != nil {
		return fmt.Errorf("while installing actor default route: %w", err)
	}

	return nil
}

// EnableIPv4Forwarding enables IPv4 forwarding in the current network namespace.
func EnableIPv4Forwarding() error {
	// Forwarding is required because actor packets now enter the worker pod via
	// the host-side veth and then leave through the pod's eth0. Without this, the
	// kernel would not route traffic between those interfaces even though both
	// live in the worker pod network namespace.
	//
	// Without privileged, the container runtime bind-mounts /proc/sys read-only.
	// The worker holds CAP_SYS_ADMIN and uses no user namespace, so the ro flag
	// is not locked: clear it, write the sysctl, restore ro.
	const path = "/proc/sys/net/ipv4/ip_forward"
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 && b[0] == '1' {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err == nil {
		return nil
	}
	if err := unix.Mount("none", "/proc/sys", "", unix.MS_BIND|unix.MS_REMOUNT, ""); err != nil {
		return fmt.Errorf("while remounting /proc/sys read-write to enable IPv4 forwarding: %w", err)
	}
	defer func() {
		_ = unix.Mount("none", "/proc/sys", "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
	}()
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("while enabling IPv4 forwarding in worker pod netns: %w", err)
	}
	return nil
}

func ipSourceEqual(ip string) []expr.Any {
	return []expr.Any{
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       12,
			Len:          4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     net.ParseIP(ip).To4(),
		},
	}
}

func l4ProtocolEqual(proto byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{proto},
		},
	}
}

// CreateNetNSWithoutSwitching creates a named netns and returns its handle,
// restoring the caller's current netns before returning.
func CreateNetNSWithoutSwitching(name string) (netns.NsHandle, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// We need to create the new NS, then switch back to the current netns.
	curNetNS, err := netns.Get()
	if err != nil {
		return -1, fmt.Errorf("while getting current netns: %w", err)
	}
	// Registered before the restoring defer below since deferred calls are LIFO.
	defer curNetNS.Close()
	defer func() {
		if err := netns.Set(curNetNS); err != nil {
			// Better to blow up the program than continue execution with
			// one OS thread randomly in a different netns.
			panic(fmt.Sprintf("Failed to restore original netns: %v", err))
		}
	}()

	interiorNetNS, err := netns.NewNamed(name)
	if err != nil {
		return -1, fmt.Errorf("while creating interior network namespace: %w", err)
	}
	return interiorNetNS, nil
}

// NetNSDo runs do() with the OS thread switched into targetNS, then restores it.
// The callback must finish namespace-sensitive work synchronously. Goroutines
// started inside it do not inherit its locked thread or target namespace; move
// serving loops and asynchronous I/O outside this call. The deferred restoration
// also runs on callback errors and panics, before the thread is unlocked.
func NetNSDo(ctx context.Context, targetNS netns.NsHandle, do func(context.Context) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Save this thread's namespace; nested calls must return to their caller's NS.
	curNetNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("while getting current netns: %w", err)
	}
	// Registered before the restoring defer below since deferred calls are LIFO.
	defer curNetNS.Close()
	defer func() {
		if err := netns.Set(curNetNS); err != nil {
			// Better to blow up the program than continue execution with
			// one OS thread randomly in a different netns.
			panic(fmt.Sprintf("Failed to restore original netns: %v", err))
		}
	}()

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("setting target netns: %w", err)
	}
	if err := do(ctx); err != nil {
		return fmt.Errorf("while executing function in target netns: %w", err)
	}
	return nil
}
