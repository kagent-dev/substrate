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
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/ateomnet"
	"github.com/agent-substrate/substrate/internal/atunnel"
	"github.com/agent-substrate/substrate/internal/roottest"
	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// TestSandboxGatewayTCP exercises the proposed gateway boundary with Linux
// applications standing in for sandbox network stacks. All proxy goroutines
// stay in this process; only their sockets belong to the gateway namespaces.
func TestSandboxGatewayTCP(t *testing.T) {
	roottest.Require(t, "veth routing, nftables REDIRECT and namespace-associated proxy sockets")
	for _, attachment := range []string{"veth", "tap"} {
		t.Run(attachment, func(t *testing.T) {
			testSandboxGatewayTCP(t, attachment)
		})
	}
}

func testSandboxGatewayTCP(t *testing.T, attachment string) {
	t.Helper()
	worker, destination := newSocketTestWorker(t)

	appAddress := netip.MustParseAddrPort(ateomnet.ActorVethIP + ":18081")
	var gateways, actors [2]*netns.NsHandle
	var stopProxy, stopApp, stopAttachment [2]func()
	var captures [2]*atomic.Int32
	for i := range gateways {
		gateways[i], actors[i], stopAttachment[i] = setupSocketTestGateway(t, *worker, i, attachment)
		addSocketTestRoute(t, *gateways[i], i, "198.51.100.10/32")
		stopProxy[i], captures[i] = serveCapturedEgress(t, *gateways[i], destination)
		stopApp[i] = serveNamespaceEcho(t, *actors[i], appAddress, fmt.Sprintf("actor%d", i))
	}
	for i := range gateways {
		// The same application IP resolves to a different actor in each gateway.
		got := exchangeNamespaceTCP(t, dialNamespaceTCP(t, *gateways[i], appAddress), "ingress")
		want := fmt.Sprintf("actor%d/%s/ingress", i, ateomnet.ActorVethGateway)
		if got != want {
			t.Fatalf("sandbox %d ingress = %q, want %q", i, got, want)
		}
		// The peer observed by the remote server must be this gateway's transit
		// IP, proving that the proxy's outbound socket uses its sandbox route.
		got = exchangeNamespaceTCP(t, dialNamespaceTCP(t, *actors[i], destination), "egress")
		want = fmt.Sprintf("remote/192.0.2.%d/egress", 4*i+2)
		if got != want {
			t.Fatalf("sandbox %d egress = %q, want %q", i, got, want)
		}
		if n := captures[i].Load(); n != 1 {
			t.Fatalf("sandbox %d captured %d connections, want one outbound connection", i, n)
		}
	}

	// Keep a B connection open while removing every part of A's network.
	connB := dialNamespaceTCP(t, *actors[1], destination)
	defer connB.Close()
	if _, err := io.WriteString(connB, "before teardown"); err != nil {
		t.Fatal(err)
	}
	want := "remote/192.0.2.6/before teardown"
	buffer := make([]byte, len(want))
	if _, err := io.ReadFull(connB, buffer); err != nil || string(buffer) != want {
		t.Fatalf("stream before teardown: %q, %v", buffer, err)
	}
	stopProxy[0]()
	stopApp[0]()
	stopAttachment[0]()
	inSocketTestNamespace(t, *gateways[0], func() error {
		return ateomnet.CleanupActorNetwork(t.Context(), *actors[0])
	})
	inSocketTestNamespace(t, *worker, func() error {
		link, err := netlink.LinkByName("sandbox0")
		if err != nil {
			return err
		}
		return netlink.LinkDel(link)
	})
	for _, ns := range []*netns.NsHandle{gateways[0], actors[0]} {
		if err := ns.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := exchangeNamespaceTCP(t, connB, "survived"); got != "survived" {
		t.Fatalf("surviving egress = %q", got)
	}
	if got := exchangeNamespaceTCP(t, dialNamespaceTCP(t, *gateways[1], appAddress), "after teardown"); got != "actor1/169.254.17.1/after teardown" {
		t.Fatalf("surviving ingress = %q", got)
	}
	if got := exchangeNamespaceTCP(t, dialNamespaceTCP(t, *actors[1], destination), "new connection"); got != "remote/192.0.2.6/new connection" {
		t.Fatalf("new egress = %q", got)
	}
	if n := captures[1].Load(); n != 3 {
		t.Fatalf("surviving sandbox captured %d connections, want 3", n)
	}
}

func setupSocketTestGateway(t *testing.T, worker netns.NsHandle, slot int, attachment string) (*netns.NsHandle, *netns.NsHandle, func()) {
	t.Helper()
	gateway, actor := setupSocketTestTransit(t, worker, slot), newSocketTestNamespace(t)
	stop := func() {}
	if attachment == "tap" {
		gatewayTap := socketTestTAP(t, *gateway, ateomnet.HostVethName, ateomnet.HostVethCIDR, "02:a8:1e:00:00:01")
		guestTap := socketTestTAP(t, *actor, ateomnet.ActorVethName, ateomnet.ActorVethCIDR, "02:a8:1e:00:00:02")
		inSocketTestNamespace(t, *actor, func() error {
			return netlink.RouteAdd(&netlink.Route{Gw: net.ParseIP(ateomnet.ActorVethGateway)})
		})
		inSocketTestNamespace(t, *gateway, func() error {
			if err := ateomnet.EnableIPv4Forwarding(); err != nil {
				return err
			}
			return ateomnet.InstallActorNftablesRules(15001)
		})
		stop = connectSocketTestTAPs(t, gatewayTap, guestTap)
	} else {
		inSocketTestNamespace(t, *gateway, func() error {
			return ateomnet.SetupActorNetwork(t.Context(), ateomnet.NetworkConfig{
				InteriorNetNS:      *actor,
				EgressRedirectPort: 15001,
			})
		})
	}
	return gateway, actor, stop
}

func newSocketTestWorker(t *testing.T) (*netns.NsHandle, netip.AddrPort) {
	t.Helper()
	worker := newSocketTestNamespace(t)
	destination := netip.MustParseAddrPort("198.51.100.10:18080")
	inSocketTestNamespace(t, *worker, func() error {
		remote := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "remote"}}
		if err := netlink.LinkAdd(remote); err != nil {
			return err
		}
		if err := configureSocketTestLink(remote, "198.51.100.10/32"); err != nil {
			return err
		}
		return configureSocketTestLink(remote, "198.51.100.20/32")
	})
	serveNamespaceEcho(t, *worker, destination, "remote")
	return worker, destination
}

func setupSocketTestTransit(t *testing.T, worker netns.NsHandle, slot int) *netns.NsHandle {
	t.Helper()
	gateway := newSocketTestNamespace(t)
	inSocketTestNamespace(t, worker, func() error {
		link := &netlink.Veth{
			LinkAttrs:     netlink.LinkAttrs{Name: fmt.Sprintf("sandbox%d", slot)},
			PeerName:      "uplink0",
			PeerNamespace: netlink.NsFd(*gateway),
		}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		return configureSocketTestLink(link, fmt.Sprintf("192.0.2.%d/30", 4*slot+1))
	})
	inSocketTestNamespace(t, *gateway, func() error {
		link, err := netlink.LinkByName("uplink0")
		if err != nil {
			return err
		}
		if err := configureSocketTestLink(link, fmt.Sprintf("192.0.2.%d/30", 4*slot+2)); err != nil {
			return err
		}
		return nil
	})
	return gateway
}

func addSocketTestRoute(t *testing.T, ns netns.NsHandle, slot int, cidr string) {
	t.Helper()
	inSocketTestNamespace(t, ns, func() error {
		_, destination, err := net.ParseCIDR(cidr)
		if err != nil {
			return err
		}
		return netlink.RouteAdd(&netlink.Route{Dst: destination, Gw: net.ParseIP(fmt.Sprintf("192.0.2.%d", slot*4+1))})
	})
}

func setupSocketTestTunnel(t *testing.T, ns netns.NsHandle, slot int) {
	t.Helper()
	addSocketTestRoute(t, ns, slot, "198.51.100.20/32")
	inSocketTestNamespace(t, ns, func() error {
		if err := ateomnet.RemoveActorNftablesRules(); err != nil {
			return err
		}
		routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Dst == nil {
				return fmt.Errorf("unexpected default route: %v", route)
			}
			if bits, _ := route.Dst.Mask.Size(); bits == 0 {
				return fmt.Errorf("unexpected default route: %v", route)
			}
		}
		return ateomnet.InstallGatewayNftablesRules(15001)
	})
}

func TestGatewayRequiresCapture(t *testing.T) {
	if err := ateomnet.InstallGatewayNftablesRules(0); err == nil {
		t.Fatal("gateway accepted a disabled proxy port")
	}
}

func TestGatewayBlocksBypass(t *testing.T) {
	roottest.Require(t, "gateway routes and fail-closed forwarding")
	worker, destination := newSocketTestWorker(t)
	gateway, actor, _ := setupSocketTestGateway(t, *worker, 0, "veth")
	setupSocketTestTunnel(t, *gateway, 0)
	// Reverse-path filtering must not hide actor traffic that escapes the gateway.
	inSocketTestNamespace(t, *worker, func() error {
		_, subnet, err := net.ParseCIDR(ateomnet.ActorVethCIDR)
		if err != nil {
			return err
		}
		return netlink.RouteAdd(&netlink.Route{Dst: subnet, Gw: net.ParseIP("192.0.2.2")})
	})
	ctx, cancel := context.WithTimeout(t.Context(), socketTestTimeout)
	defer cancel()
	if conn, err := ateomnet.DialTCP(ctx, *gateway, destination); err == nil {
		conn.Close()
		t.Fatal("gateway can connect directly to the application destination")
	} else if !errors.Is(err, unix.ENETUNREACH) {
		t.Fatalf("unrouted destination: %v", err)
	}
	for _, host := range []string{"192.0.2.1", "198.51.100.20"} {
		t.Run(host, func(t *testing.T) {
			address := &net.UDPAddr{IP: net.ParseIP(host), Port: 18082}
			var listener *net.UDPConn
			inSocketTestNamespace(t, *worker, func() error {
				var err error
				listener, err = net.ListenUDP("udp4", address)
				return err
			})
			defer listener.Close()
			send := func(ns netns.NsHandle, message string) {
				inSocketTestNamespace(t, ns, func() error {
					conn, err := net.DialUDP("udp4", nil, address)
					if err != nil {
						return err
					}
					defer conn.Close()
					_, err = conn.Write([]byte(message))
					return err
				})
			}
			// The gateway has a route to both the connected peer and the
			// allowlisted egress endpoint. Only forwarded actor traffic is denied.
			send(*gateway, "local")
			buffer := make([]byte, 32)
			_ = listener.SetReadDeadline(time.Now().Add(socketTestTimeout))
			if n, _, err := listener.ReadFromUDP(buffer); err != nil || string(buffer[:n]) != "local" {
				t.Fatalf("control UDP: %q, %v", buffer[:n], err)
			}
			// Prove the same actor path works without the policy before asserting
			// that the gateway firewall blocks it, including after reinstallation.
			inSocketTestNamespace(t, *gateway, func() error {
				c := &nftables.Conn{}
				c.DelTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "ateom_gateway"})
				return c.Flush()
			})
			send(*actor, "control")
			_ = listener.SetReadDeadline(time.Now().Add(socketTestTimeout))
			if n, _, err := listener.ReadFromUDP(buffer); err != nil || string(buffer[:n]) != "control" {
				t.Fatalf("actor control UDP: %q, %v", buffer[:n], err)
			}
			for range 2 {
				inSocketTestNamespace(t, *gateway, func() error { return ateomnet.InstallGatewayNftablesRules(15001) })
			}
			send(*actor, "bypass")
			_ = listener.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if n, _, err := listener.ReadFromUDP(buffer); err == nil {
				t.Fatalf("forwarded UDP escaped: %q", buffer[:n])
			} else if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func socketTestTAP(t *testing.T, ns netns.NsHandle, name, cidr, mac string) *os.File {
	t.Helper()
	tap := &netlink.Tuntap{
		LinkAttrs:  netlink.LinkAttrs{Name: name, HardwareAddr: ateomnet.MustParseMAC(mac), MTU: 1500},
		Mode:       netlink.TUNTAP_MODE_TAP,
		Flags:      netlink.TUNTAP_NO_PI | netlink.TUNTAP_VNET_HDR,
		Queues:     1,
		NonPersist: true,
	}
	inSocketTestNamespace(t, ns, func() error {
		if err := netlink.LinkAdd(tap); err != nil {
			return err
		}
		t.Cleanup(func() { _ = tap.Fds[0].Close() })
		return configureSocketTestLink(tap, cidr)
	})
	return tap.Fds[0]
}

// The second TAP lets a real Linux TCP stack stand in for the guest kernel.
// Copying frames between the FDs emulates the VMM's Ethernet transport without
// implementing a TCP stack in the test. The gateway uses no TC redirect.
func connectSocketTestTAPs(t *testing.T, a, b *os.File) func() {
	t.Helper()
	var wg sync.WaitGroup
	for _, pair := range [][2]*os.File{{a, b}, {b, a}} {
		wg.Go(func() {
			buffer := make([]byte, 65536)
			for {
				n, err := pair[0].Read(buffer)
				if err != nil {
					if !errors.Is(err, os.ErrClosed) {
						t.Errorf("reading TAP: %v", err)
					}
					return
				}
				written, err := pair[1].Write(buffer[:n])
				if err != nil {
					if !errors.Is(err, os.ErrClosed) {
						t.Errorf("writing TAP: %v", err)
					}
					return
				}
				if written != n {
					t.Error(io.ErrShortWrite)
					return
				}
			}
		})
	}
	stop := sync.OnceFunc(func() {
		_ = a.Close()
		_ = b.Close()
		wg.Wait()
	})
	t.Cleanup(stop)
	return stop
}

func configureSocketTestLink(link netlink.Link, cidr string) error {
	if err := netlink.AddrAdd(link, ateomnet.MustParseAddr(cidr)); err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}

func serveCapturedEgress(t *testing.T, ns netns.NsHandle, destination netip.AddrPort) (func(), *atomic.Int32) {
	t.Helper()
	return serveCapturedEgressDialer(t, ns, destination, namespaceEgressDialer(ns))
}

func serveCapturedEgressDialer(t *testing.T, ns netns.NsHandle, destination netip.AddrPort, dialer interface {
	DialContext(context.Context, string) (net.Conn, error)
}) (func(), *atomic.Int32) {
	t.Helper()
	var captures atomic.Int32
	egress, err := atunnel.NewEgress(func(conn net.Conn) (string, error) {
		captures.Add(1)
		original, err := atunnel.TCPOriginalDestination(conn)
		if err == nil && original != destination.String() {
			err = fmt.Errorf("original destination = %s, want %s", original, destination)
			t.Error(err)
		}
		return original, err
	})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := ateomnet.ListenTCP(ns, netip.MustParseAddrPort("0.0.0.0:15001"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	if err := egress.Activate(dialer, socketTestCertificate{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- egress.Serve(ctx, lis) }()
	stop := sync.OnceFunc(func() {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), socketTestTimeout)
		defer cleanupCancel()
		if err := egress.Deactivate(cleanupCtx); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(stop)
	return stop, &captures
}

// The kernel-path test uses a direct TCP upstream. TLS CONNECT and certificate
// behavior are covered separately by internal/atunnel's tests.
type namespaceEgressDialer netns.NsHandle

func (ns namespaceEgressDialer) DialContext(ctx context.Context, destination string) (net.Conn, error) {
	address, err := netip.ParseAddrPort(destination)
	if err != nil {
		return nil, err
	}
	return ateomnet.DialTCP(ctx, netns.NsHandle(ns), address)
}

type socketTestCertificate struct{}

func (socketTestCertificate) MintAteomCertificate(context.Context) (time.Time, error) {
	return time.Now().Add(time.Hour), nil
}
