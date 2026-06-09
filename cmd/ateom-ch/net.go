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
	"log/slog"

	"github.com/vishvananda/netlink"
)

const tapMTU = 1500

// tapName returns a deterministic, valid (≤15 char) tap interface name for an actor.
// Uses the first 8 chars of actorID (UUIDs are hex-dense at the start).
func tapName(actorID string) string {
	suffix := actorID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return "atch-" + suffix
}

// createTap creates a tap device in the current network namespace and brings it up.
// Returns the tap name.
func createTap(ctx context.Context, actorID string) (string, error) {
	name := tapName(actorID)

	// Remove stale tap from a prior failed run.
	if old, err := netlink.LinkByName(name); err == nil {
		if delErr := netlink.LinkDel(old); delErr != nil {
			return "", fmt.Errorf("removing stale tap %q: %w", name, delErr)
		}
	}

	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
			MTU:  tapMTU,
		},
		Mode:  netlink.TUNTAP_MODE_TAP,
		Flags: netlink.TUNTAP_ONE_QUEUE | netlink.TUNTAP_NO_PI,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return "", fmt.Errorf("creating tap %q: %w", name, err)
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", fmt.Errorf("looking up tap %q after create: %w", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return "", fmt.Errorf("bringing up tap %q: %w", name, err)
	}

	// Bridge tap to eth0 so the VM gets pod-level network connectivity.
	eth0, err := netlink.LinkByName("eth0")
	if err != nil {
		return "", fmt.Errorf("looking up eth0: %w", err)
	}
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeName(actorID),
			MTU:  tapMTU,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		return "", fmt.Errorf("creating bridge: %w", err)
	}
	br, err := netlink.LinkByName(bridgeName(actorID))
	if err != nil {
		return "", fmt.Errorf("looking up bridge: %w", err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return "", fmt.Errorf("bringing up bridge: %w", err)
	}
	if err := netlink.LinkSetMaster(eth0, br); err != nil {
		return "", fmt.Errorf("adding eth0 to bridge: %w", err)
	}
	if err := netlink.LinkSetMaster(link, br); err != nil {
		return "", fmt.Errorf("adding tap to bridge: %w", err)
	}

	slog.InfoContext(ctx, "Tap created and bridged to eth0", slog.String("tap", name))
	return name, nil
}

// deleteTap removes the tap device and associated bridge, restoring eth0.
func deleteTap(ctx context.Context, actorID string) error {
	brName := bridgeName(actorID)
	tapName := tapName(actorID)

	// Remove eth0 from bridge before deleting it.
	if eth0, err := netlink.LinkByName("eth0"); err == nil {
		_ = netlink.LinkSetNoMaster(eth0)
	}

	if tap, err := netlink.LinkByName(tapName); err == nil {
		if err := netlink.LinkDel(tap); err != nil {
			slog.WarnContext(ctx, "Failed to delete tap", slog.String("tap", tapName), slog.Any("err", err))
		}
	}
	if br, err := netlink.LinkByName(brName); err == nil {
		if err := netlink.LinkDel(br); err != nil {
			slog.WarnContext(ctx, "Failed to delete bridge", slog.String("bridge", brName), slog.Any("err", err))
		}
	}
	slog.InfoContext(ctx, "Tap and bridge deleted", slog.String("actor", actorID))
	return nil
}

func bridgeName(actorID string) string {
	suffix := actorID
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	return "atchbr-" + suffix
}
