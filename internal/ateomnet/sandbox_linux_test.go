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

package ateomnet_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/ateomnet"
	"github.com/agent-substrate/substrate/internal/roottest"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestSandboxSetupRollsBackPartialVeth(t *testing.T) {
	roottest.Require(t, "sandbox network setup rollback")
	gateway, runtime := newSocketTestNamespace(t), newSocketTestNamespace(t)
	n := &ateomnet.Sandbox{Gateway: *gateway, Runtime: *runtime, MTU: 1500}
	inSocketTestNamespace(t, *runtime, func() error {
		lo, err := netlink.LinkByName("lo")
		if err != nil {
			return err
		}
		if err := netlink.LinkSetDown(lo); err != nil {
			return err
		}
		return netlink.LinkSetName(lo, "broken-lo")
	})
	if err := n.Setup(t.Context(), 15001); err == nil || !strings.Contains(err.Error(), "lo in interior netns") {
		t.Fatalf("Setup without runtime loopback = %v", err)
	}
	for ns, name := range map[netns.NsHandle]string{*gateway: ateomnet.HostVethName, *runtime: ateomnet.ActorVethName} {
		inSocketTestNamespace(t, ns, func() error {
			_, err := netlink.LinkByName(name)
			if _, ok := errors.AsType[netlink.LinkNotFoundError](err); !ok {
				return fmt.Errorf("partial interface %s remains after failed setup: %v", name, err)
			}
			return nil
		})
	}
	inSocketTestNamespace(t, *runtime, func() error {
		lo, err := netlink.LinkByName("broken-lo")
		if err != nil {
			return err
		}
		return netlink.LinkSetName(lo, "lo")
	})
	if err := n.Setup(t.Context(), 15001); err != nil {
		t.Fatalf("retry Setup: %v", err)
	}
	address := netip.MustParseAddrPort(ateomnet.ActorVethIP + ":8081")
	serveNamespaceEcho(t, *runtime, address, "retry")
	conn, err := n.DialContext(t.Context(), "tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	if got := exchangeNamespaceTCP(t, conn.(*net.TCPConn), "ready"); !strings.HasPrefix(got, "retry/") {
		t.Fatalf("retry connectivity = %q", got)
	}
}

type sandboxEndpointDialer struct{ n *ateomnet.Sandbox }

func (d sandboxEndpointDialer) DialContext(ctx context.Context, address string) (net.Conn, error) {
	return d.n.DialEndpoint(ctx, "tcp", address)
}

func TestProductionSandboxNetwork(t *testing.T) {
	roottest.Require(t, "production sandbox topology")
	worker, destination := newSocketTestWorker(t)
	inSocketTestNamespace(t, *worker, func() error {
		return netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", MTU: 1400}})
	})
	var sandboxes [2]*ateomnet.Sandbox
	var stop [2]func()
	for i := range sandboxes {
		var n *ateomnet.Sandbox
		inSocketTestNamespace(t, *worker, func() error {
			var err error
			n, err = ateomnet.NewSandbox(t.Context(), fmt.Sprintf("%s-%d", t.Name(), i), false)
			return err
		})
		sandboxes[i] = n
		t.Cleanup(func() { inSocketTestNamespace(t, *worker, n.Close) })
		if n.MTU != 1400 {
			t.Fatalf("MTU %d", n.MTU)
		}
		stop[i], _ = serveCapturedEgressDialer(t, n.Gateway, destination, sandboxEndpointDialer{n})
		if err := n.Setup(t.Context(), 15001); err != nil {
			t.Fatal(err)
		}
		got := exchangeNamespaceTCP(t, dialNamespaceTCP(t, n.Runtime, destination), "production")
		if !strings.HasSuffix(got, "/production") {
			t.Fatalf("echo %q", got)
		}
		inSocketTestNamespace(t, n.Gateway, func() error {
			routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
			if err != nil {
				return err
			}
			for _, r := range routes {
				if r.Dst == nil || r.Dst.String() == "0.0.0.0/0" || r.Dst.String() == "::/0" {
					return fmt.Errorf("default route: %s", r)
				}
			}
			return nil
		})
		// Connected routes serve ingress to the right sandbox despite identical IPs.
		serveNamespaceEcho(t, n.Runtime, netip.MustParseAddrPort("169.254.17.2:8081"), fmt.Sprintf("actor%d", i))
		c, err := n.DialContext(t.Context(), "tcp", "169.254.17.2:8081")
		if err != nil {
			t.Fatal(err)
		}
		got = exchangeNamespaceTCP(t, c.(*net.TCPConn), "ingress")
		if !strings.HasPrefix(got, fmt.Sprintf("actor%d/", i)) {
			t.Fatalf("ingress %q", got)
		}
	}
	// Duplicate enrollment must not remove the live namespace.
	inSocketTestNamespace(t, *worker, func() error {
		_, err := ateomnet.NewSandbox(t.Context(), t.Name()+"-0", false)
		if err == nil {
			return fmt.Errorf("duplicate enrollment succeeded")
		}
		return nil
	})
	if c, err := sandboxes[0].DialContext(t.Context(), "tcp", "169.254.17.2:8081"); err != nil {
		t.Fatal(err)
	} else if got := exchangeNamespaceTCP(t, c.(*net.TCPConn), "still-enrolled"); !strings.HasPrefix(got, "actor0/") {
		t.Fatalf("duplicate enrollment damaged ingress: %q", got)
	}
	stop[0]()
	inSocketTestNamespace(t, *worker, sandboxes[0].Close)
	if _, err := sandboxes[0].DialContext(t.Context(), "tcp", "169.254.17.2:8081"); err == nil {
		t.Fatal("closed namespace admitted socket")
	}
	got := exchangeNamespaceTCP(t, dialNamespaceTCP(t, sandboxes[1].Runtime, destination), "survives")
	if !strings.HasSuffix(got, "/survives") {
		t.Fatalf("peer teardown: %q", got)
	}
}
