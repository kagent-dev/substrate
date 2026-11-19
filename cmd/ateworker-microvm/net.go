//go:build linux

// Copyright 2026 The Agent Substrate Authors
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
	"log/slog"
	"os"

	"github.com/vishvananda/netlink"

	"go.ate.dev/substrate/internal/ateworker/actornet"
	"go.ate.dev/substrate/internal/netns"
)

const (
	// gatewayMAC is deliberately FIXED (locally administered), unlike
	// ateworker-gvisor where a random veth MAC is fine. A CH snapshot freezes the
	// guest kernel's neighbor cache, including the entries for the gateway's
	// addresses; restoring against a tap with a fresh random MAC would
	// blackhole guest egress until those entries expired.
	gatewayMAC = "02:a8:1e:00:00:01"

	// actorGuestMAC is the FIXED MAC for the guest's eth0 (the CH virtio-net).
	// Fixed for the same reason as gatewayMAC: a cold boot freezes this MAC into
	// the guest+snapshot, and restore re-adds the virtio-net under the same MAC
	// (SnapshotNetDevices reads it back), so the guest's frozen interface config
	// stays valid across pods. Distinct from the gateway MAC (…:01).
	actorGuestMAC = "02:a8:1e:00:00:02"

	// actorTapMTU is the kernel default the guest was snapshotted against.
	actorTapMTU = 1500
)

var gatewayHWAddr = actornet.MustParseMAC(gatewayMAC)

// setupActorTap creates the guest's tap with fixed gateway addresses and MAC.
// Returns the FDs cloud-hypervisor adopts on boot or restore.
func setupActorTap(ctx context.Context, actorNetNS netns.Handle, name string, queuePairs int, families actornet.Families) ([]*os.File, error) {
	var fds []*os.File
	err := netns.Do(ctx, actorNetNS, func(ctx context.Context) error {
		if old, lerr := netlink.LinkByName(name); lerr == nil {
			_ = netlink.LinkDel(old)
		}
		flags := netlink.TUNTAP_NO_PI | netlink.TUNTAP_VNET_HDR
		if queuePairs > 1 {
			flags |= netlink.TUNTAP_MULTI_QUEUE
		}
		tap := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: name, MTU: actorTapMTU},
			Mode:      netlink.TUNTAP_MODE_TAP,
			Flags:     flags,
			Queues:    queuePairs,
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("creating tap %q: %w", name, err)
		}
		fds = tap.Fds
		// Set the MAC after LinkAdd; tuntap creation ignores LinkAttrs.HardwareAddr.
		// It must match the guest's snapshotted ARP entry.
		link, err := netlink.LinkByName(name)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetHardwareAddr(link, gatewayHWAddr); err != nil {
			return fmt.Errorf("setting the gateway MAC on tap %q: %w", name, err)
		}
		for _, addr := range families.GatewayAddrs() {
			if err := netlink.AddrReplace(link, addr); err != nil {
				return fmt.Errorf("assigning the gateway address %s to tap %q: %w", addr, name, err)
			}
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("bringing up tap %q: %w", name, err)
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

// actorTapMTUOf reads the tap MTU, falling back to actorTapMTU on error.
func actorTapMTUOf(ctx context.Context, actorNetNS netns.Handle, name string) int {
	mtu := actorTapMTU
	_ = netns.Do(ctx, actorNetNS, func(ctx context.Context) error {
		if l, err := netlink.LinkByName(name); err == nil {
			mtu = l.Attrs().MTU
		} else {
			slog.WarnContext(ctx, "Failed to read actor tap MTU; using default",
				slog.String("link", name), slog.Int("default_mtu", mtu), slog.Any("err", err))
		}
		return nil
	})
	return mtu
}
