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

// These integration tests use an external package so that actornet's own test
// binary does not depend on atunnel.
package actornet_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"go.ate.dev/substrate/internal/ateworker/actornet"
	"go.ate.dev/substrate/internal/linuxsys"
	"go.ate.dev/substrate/internal/netns"
	"go.ate.dev/substrate/internal/roottest"
)

var testFamilies = []struct {
	name     string
	families actornet.Families
}{
	{"ipv4", actornet.Families{IPv4: true}},
	{"ipv6", actornet.Families{IPv6: true}},
	{"dual", actornet.Families{IPv4: true, IPv6: true}},
}

// inFamilies reports whether destination, a host:port, is in one of families.
func inFamilies(t *testing.T, families actornet.Families, destination string) bool {
	t.Helper()
	ap, err := netip.ParseAddrPort(destination)
	if err != nil {
		t.Fatal(err)
	}
	if ap.Addr().Is4() {
		return families.IPv4
	}
	return families.IPv6
}

// waitOperUp waits until the sandbox's veth ends are operationally up. Until
// the kernel's link watch activates a new veth it drops what is sent on it,
// and the dial would wait out a SYN retransmit.
func waitOperUp(t *testing.T, n *actornet.SandboxNetwork) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, end := range []struct {
		ns   netns.Handle
		name string
	}{{n.RuntimeNetNS, actornet.ActorVethName}, {n.GatewayNetNS, "atside"}} {
		if err := netns.Do(ctx, end.ns, func(ctx context.Context) error {
			updates := make(chan netlink.LinkUpdate)
			done := make(chan struct{})
			defer close(done)
			if err := netlink.LinkSubscribe(updates, done); err != nil {
				return err
			}
			for {
				link, err := netlink.LinkByName(end.name)
				if err != nil {
					return err
				}
				if link.Attrs().OperState == netlink.OperUp {
					return nil
				}
				select {
				case <-updates:
				case <-ctx.Done():
					return fmt.Errorf("%s is %s: %w", end.name, link.Attrs().OperState, ctx.Err())
				}
			}
		}); err != nil {
			t.Fatalf("waiting for the veth: %v", err)
		}
	}
}

// Verify redirection preserves the destination, including unconfigured ports,
// and that a family the sandbox lacks has no egress at all.
func TestSandboxEgressReachesAtunnelOnAnyPort(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	for _, tc := range testFamilies {
		t.Run(tc.name, func(t *testing.T) { testSandboxEgressReachesAtunnel(t, tc.families) })
	}
}

func testSandboxEgressReachesAtunnel(t *testing.T, families actornet.Families) {
	ctx := context.Background()
	const egressPort = 15001

	n, err := actornet.SetupSandboxNetwork(ctx, actornet.SandboxNetworkConfig{
		ActorUID: "66666666-6666-6666-6666-666666666666", Veth: true, Families: families, EgressPort: egressPort,
	})
	if err != nil {
		t.Fatalf("SetupSandboxNetwork: %v", err)
	}
	t.Cleanup(func() { actornet.CleanupSandboxNetwork(n) })
	waitOperUp(t, n)

	listeners, err := netns.Listen(ctx, n.GatewayNetNS, []uint16{egressPort})
	if err != nil {
		t.Fatalf("netns.Listen: %v", err)
	}
	defer listeners[0].Close()

	type capture struct{ destination, payload string }
	got := make(chan capture, 1)
	accept := func() {
		c, err := listeners[0].Accept()
		if err != nil {
			return
		}
		defer c.Close()
		dst, err := linuxsys.TCPOriginalDestination(c)
		if err != nil {
			t.Errorf("TCPOriginalDestination: %v", err)
			return
		}
		buf := make([]byte, len("hello"))
		io.ReadFull(c, buf)
		got <- capture{destination: dst, payload: string(buf)}
	}
	go accept()

	for _, want := range []string{"93.184.216.34:443", "93.184.216.34:8080", "93.184.216.34:9999", "[2001:db8::1]:443", "[2001:db8::1]:9999"} {
		dialErr := netns.Do(ctx, n.RuntimeNetNS, func(context.Context) error {
			c, err := net.DialTimeout("tcp", want, 3*time.Second)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = io.WriteString(c, "hello")
			return err
		})
		if !inFamilies(t, families, want) {
			if !errors.Is(dialErr, syscall.ENETUNREACH) {
				t.Errorf("dial to %s outside the sandbox's families = %v, want network unreachable", want, dialErr)
			}
			continue
		}
		if dialErr != nil {
			t.Fatalf("sandbox egress dial to %s: %v", want, dialErr)
		}
		select {
		case c := <-got:
			if c.destination != want {
				t.Errorf("atunnel saw destination %q, want %q", c.destination, want)
			}
			if c.payload != "hello" {
				t.Errorf("atunnel read %q, want %q", c.payload, "hello")
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the sandbox's connection to %s never reached atunnel", want)
		}
		go accept()
	}
}

func TestSandboxLinkLocalDoesNotReachAtunnel(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	for _, tc := range testFamilies {
		t.Run(tc.name, func(t *testing.T) { testSandboxLinkLocalDoesNotReachAtunnel(t, tc.families) })
	}
}

func testSandboxLinkLocalDoesNotReachAtunnel(t *testing.T, families actornet.Families) {
	ctx := context.Background()
	const egressPort = 15001

	n, err := actornet.SetupSandboxNetwork(ctx, actornet.SandboxNetworkConfig{
		ActorUID:   "99999999-9999-9999-9999-999999999999",
		Veth:       true,
		Families:   families,
		EgressPort: egressPort,
	})
	if err != nil {
		t.Fatalf("SetupSandboxNetwork: %v", err)
	}
	t.Cleanup(func() { actornet.CleanupSandboxNetwork(n) })
	waitOperUp(t, n)

	listeners, err := netns.Listen(ctx, n.GatewayNetNS, []uint16{egressPort})
	if err != nil {
		t.Fatalf("netns.Listen: %v", err)
	}
	defer listeners[0].Close()
	listener := listeners[0].(*net.TCPListener)

	// fe80::/10 is on-link, so the actor never routes it to the gateway.
	for _, rejected := range []string{"169.254.169.254:80", "[fd00:ec2::254]:80", "[fd20:ce::254]:80"} {
		if !inFamilies(t, families, rejected) {
			continue
		}
		var dialErr error
		if err := netns.Do(ctx, n.RuntimeNetNS, func(context.Context) error {
			// Under the 1s SYN retransmit, so a dropped SYN times out.
			conn, err := net.DialTimeout("tcp", rejected, 750*time.Millisecond)
			dialErr = err
			if conn != nil {
				conn.Close()
			}
			return nil
		}); err != nil {
			t.Fatalf("dial to %s: %v", rejected, err)
		}
		if dialErr == nil {
			t.Fatalf("dial to %s succeeded", rejected)
		}
		if timeout, ok := dialErr.(net.Error); ok && timeout.Timeout() {
			t.Fatalf("dial to %s timed out instead of being rejected: %v", rejected, dialErr)
		}
		if err := listener.SetDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if conn, err := listener.Accept(); err == nil {
			conn.Close()
			t.Fatalf("connection to %s reached atunnel", rejected)
		} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
			t.Fatalf("Accept after dialing %s: %v", rejected, err)
		}
	}

	if err := listener.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []string{"192.0.2.1:443", "[2001:db8::1]:443"} {
		if !inFamilies(t, families, allowed) {
			continue
		}
		if err := netns.Do(ctx, n.RuntimeNetNS, func(context.Context) error {
			conn, err := net.Dial("tcp", allowed)
			if err != nil {
				return err
			}
			return conn.Close()
		}); err != nil {
			t.Fatalf("dial to allowed CIDR: %v", err)
		}
		conn, err := listener.Accept()
		if err != nil {
			t.Fatalf("accept allowed connection: %v", err)
		}
		destination, err := linuxsys.TCPOriginalDestination(conn)
		conn.Close()
		if err != nil {
			t.Fatalf("TCPOriginalDestination: %v", err)
		}
		if destination != allowed {
			t.Errorf("atunnel saw destination %q, want %q", destination, allowed)
		}
	}
}
