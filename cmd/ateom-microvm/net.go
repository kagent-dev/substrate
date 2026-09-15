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

package main

import (
	"context"
	"fmt"

	"os"

	"github.com/vishvananda/netlink"

	"github.com/agent-substrate/substrate/internal/ateomnet"
)

const (
	// hostVethMAC is deliberately FIXED (locally administered), unlike
	// ateom-gvisor where the kernel's random veth MAC is fine. A CH snapshot
	// freezes the guest kernel's ARP cache, including the entry for the
	// gateway 169.254.17.1; restoring against a new veth pair with a random
	// MAC would blackhole guest egress until that entry expires. A constant
	// gateway MAC keeps the frozen entry valid on every pod.
	hostVethMAC = "02:a8:1e:00:00:01"

	// actorGuestMAC is the FIXED MAC for the guest's eth0 (the CH virtio-net).
	// Fixed for the same reason as hostVethMAC: a cold boot freezes this MAC into
	// the guest+snapshot, and restore re-adds the
	// virtio-net under the same MAC (SnapshotNetDevices reads it back), so the
	// guest's frozen interface config stays valid across pods. Distinct from the
	// gateway MAC (…:01).
	actorGuestMAC = "02:a8:1e:00:00:02"
)

var (
	hostVethHWAddr = ateomnet.MustParseMAC(hostVethMAC)
)

// setupTap creates the sandbox-facing TAP directly in its gateway.
// Cloud Hypervisor receives the queue FDs; proxy sockets use the same namespace.
// Guest frames written by CH enter Gateway through this TAP and hit PREROUTING
// capture, just as gVisor frames arrive through its veth. The caller closes the
// queue FDs after passing them to CH; Proxy.Reset removes the device on teardown.
func (s *AteomService) setupTap(ctx context.Context, name string, queuePairs int) ([]*os.File, error) {
	var fds []*os.File
	err := ateomnet.NetNSDo(ctx, s.proxy.Net.Gateway, func(ctx context.Context) error {

		if old, lerr := netlink.LinkByName(name); lerr == nil {
			_ = netlink.LinkDel(old)
		}
		flags := netlink.TUNTAP_NO_PI | netlink.TUNTAP_VNET_HDR
		if queuePairs > 1 {
			flags |= netlink.TUNTAP_MULTI_QUEUE
		}
		tap := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: name},
			Mode:      netlink.TUNTAP_MODE_TAP,
			Flags:     flags,
			Queues:    queuePairs,
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("creating tap %q: %w", name, err)
		}
		fds = tap.Fds
		// TUNSETIFF does not apply LinkAttrs MAC/MTU. Kata pins the gateway's
		// neighbor entry, so set both explicitly before bringing the TAP up.
		if err := netlink.LinkSetHardwareAddr(tap, hostVethHWAddr); err != nil {
			return err
		}
		if err := netlink.LinkSetMTU(tap, s.proxy.Net.MTU); err != nil {
			return err
		}
		if err := netlink.LinkSetUp(tap); err != nil {
			return fmt.Errorf("bringing up tap %q: %w", name, err)
		}
		if err := netlink.AddrReplace(tap, ateomnet.HostVethAddr); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		for _, f := range fds {
			_ = f.Close()
		}
		return nil, err
	}
	return fds, nil
}
