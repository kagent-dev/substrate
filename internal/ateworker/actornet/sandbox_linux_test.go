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

package actornet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/dns/dnsmessage"

	"go.ate.dev/substrate/internal/atunnel/dns"
	"go.ate.dev/substrate/internal/nodepath"

	"go.ate.dev/substrate/internal/netns"
	"go.ate.dev/substrate/internal/roottest"
)

const testEgressPort = 15001

var dualStack = Families{IPv4: true, IPv6: true}

// testFamilies are the sandbox shapes every family-dependent test covers.
var testFamilies = []struct {
	name     string
	families Families
}{
	{"ipv4", Families{IPv4: true}},
	{"ipv6", Families{IPv6: true}},
	{"dual", dualStack},
}

func TestPodFamilies(t *testing.T) {
	for _, tc := range []struct {
		podIPs  []string
		want    Families
		wantErr bool
	}{
		{podIPs: []string{"10.0.0.1"}, want: Families{IPv4: true}},
		{podIPs: []string{"fd00::1"}, want: Families{IPv6: true}},
		{podIPs: []string{"10.0.0.1", "fd00::1"}, want: dualStack},
		{podIPs: []string{"::ffff:10.0.0.1"}, want: Families{IPv4: true}},
		{podIPs: nil, wantErr: true},
		{podIPs: []string{"not-an-ip"}, wantErr: true},
	} {
		got, err := PodFamilies(tc.podIPs)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("PodFamilies(%q) = %+v, %v; want %+v, error %v", tc.podIPs, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestSetupSandboxNetworkNeedsAFamily(t *testing.T) {
	if _, err := SetupSandboxNetwork(context.Background(), SandboxNetworkConfig{ActorUID: "no-family", EgressPort: testEgressPort}); err == nil {
		t.Fatal("SetupSandboxNetwork with no IP family succeeded")
	}
}

// The actor's resolv.conf names the gateway in each of its families, so the
// relay must answer on every one of them, over UDP and TCP. A nil policy
// refuses every name, so the answer is NXDOMAIN and the upstream sees no
// query. TCP still connects to it for each client.
func TestSandboxDNSAnswersOnEveryGatewayAddress(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upstream.Close() })
	go func() {
		for {
			c, err := upstream.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	relay, err := dns.NewRelayForUpstreams([]string{upstream.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x5ad, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	query, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range testFamilies {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			session, err := ServeSandbox(ctx, SandboxNetworkConfig{
				ActorUID: "dns-" + tc.name, Veth: true, Families: tc.families, EgressPort: testEgressPort,
			}, nil, relay)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close(context.Background()) })

			for _, nameserver := range tc.families.gatewayIPs() {
				for _, network := range []string{"udp", "tcp"} {
					var answer []byte
					if err := netns.Do(ctx, session.Network.RuntimeNetNS, func(context.Context) error {
						c, err := net.DialTimeout(network, net.JoinHostPort(nameserver, "53"), 5*time.Second)
						if err != nil {
							return err
						}
						defer c.Close()
						_ = c.SetDeadline(time.Now().Add(5 * time.Second))
						msg := query
						if network == "tcp" {
							msg = append([]byte{byte(len(query) >> 8), byte(len(query))}, query...)
						}
						if _, err := c.Write(msg); err != nil {
							return err
						}
						buf := make([]byte, 512)
						n, err := c.Read(buf)
						if err != nil {
							return err
						}
						answer = buf[:n]
						if network == "tcp" {
							answer = answer[2:]
						}
						return nil
					}); err != nil {
						t.Fatalf("querying %s over %s: %v", nameserver, network, err)
					}
					var p dnsmessage.Parser
					h, err := p.Start(answer)
					if err != nil {
						t.Fatalf("parsing the answer from %s over %s: %v", nameserver, network, err)
					}
					if h.ID != 0x5ad || h.RCode != dnsmessage.RCodeNameError {
						t.Errorf("%s over %s answered id %#x rcode %v, want id 0x5ad NXDOMAIN", nameserver, network, h.ID, h.RCode)
					}
				}
			}
		})
	}
}

func TestSandboxSessionDialerAfterClose(t *testing.T) {
	session := &SandboxSession{}
	dial := session.Dialer()
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:1"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("dial after close: got %v, want closed", err)
	}
}

func TestSandboxSessionDialerConcurrentClose(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	session, err := ServeSandbox(context.Background(), SandboxNetworkConfig{
		ActorUID: "concurrent-close", Families: dualStack, EgressPort: testEgressPort,
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	dial := session.Dialer()
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range 32 {
				conn, err := dial(context.Background(), "udp", "127.0.0.1:9")
				if err != nil {
					if !errors.Is(err, net.ErrClosed) {
						t.Errorf("dial during close: %v", err)
					}
					return
				}
				_ = conn.Close()
			}
		}()
	}
	close(start)
	if err := session.Close(context.Background()); err != nil {
		t.Error(err)
	}
	workers.Wait()
}

func TestSetupSandboxNetwork(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	for _, tc := range testFamilies {
		t.Run(tc.name, func(t *testing.T) { testSetupSandboxNetwork(t, tc.families) })
	}
}

func testSetupSandboxNetwork(t *testing.T, families Families) {
	ctx := context.Background()
	actorAddr := net.JoinHostPort(families.ActorIP(), "80")

	type actor struct {
		net  *SandboxNetwork
		body string
	}
	actors := map[string]*actor{}
	for _, uid := range []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"} {
		n, err := SetupSandboxNetwork(ctx, SandboxNetworkConfig{ActorUID: uid, Veth: true, Families: families, EgressPort: testEgressPort})
		if err != nil {
			t.Fatalf("SetupSandboxNetwork(%s): %v", uid, err)
		}
		t.Cleanup(func() {
			if err := CleanupSandboxNetwork(n); err != nil {
				t.Errorf("cleanup %s: %v", uid, err)
			}
		})

		// The actor's app, bound where a real one binds, inside its namespace.
		var lis net.Listener
		if err := netns.Do(ctx, n.RuntimeNetNS, func(context.Context) error {
			l, err := net.Listen("tcp", actorAddr)
			lis = l
			return err
		}); err != nil {
			t.Fatalf("actor %s listen: %v", uid, err)
		}
		t.Cleanup(func() { lis.Close() })
		body := "i-am-" + uid[:8]
		go http.Serve(lis, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		actors[uid] = &actor{net: n, body: body}
	}

	// Reaching each actor is a matter of which namespace the dial is made from.
	for uid, a := range actors {
		client := &http.Client{Transport: &http.Transport{DialContext: netns.Dialer(a.net.RuntimeNetNS)}, Timeout: 5 * time.Second}
		resp, err := client.Get((&url.URL{Scheme: "http", Host: actorAddr}).String())
		if err != nil {
			t.Fatalf("reaching actor %s: %v", uid, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != a.body {
			t.Errorf("actor %s answered %q, want %q", uid, got, a.body)
		}
	}

	// Sandbox addresses must not be reachable from the worker namespace.
	direct := &http.Client{Timeout: 2 * time.Second}
	if _, err := direct.Get("http://" + actorAddr); err == nil {
		t.Error("the worker namespace reached an actor directly; addresses are not isolated")
	}

	// The actor has addresses and a default route in its families only.
	for _, a := range actors {
		if err := netns.Do(ctx, a.net.RuntimeNetNS, func(context.Context) error {
			eth0, err := netlink.LinkByName(ActorVethName)
			if err != nil {
				return err
			}
			for _, f := range []struct {
				family int
				want   bool
				addr   string
			}{
				{netlink.FAMILY_V4, families.IPv4, ActorVethIP},
				{netlink.FAMILY_V6, families.IPv6, ActorVethIPv6},
			} {
				addrs, err := netlink.AddrList(eth0, f.family)
				if err != nil {
					return err
				}
				var found bool
				for _, addr := range addrs {
					// The kernel adds an IPv6 link-local address of its own.
					if addr.IP.To4() == nil && addr.IP.IsLinkLocalUnicast() {
						continue
					}
					if addr.IP.String() != f.addr {
						t.Errorf("unexpected address %s on the actor's %s", addr.IP, ActorVethName)
					}
					found = true
				}
				if found != f.want {
					t.Errorf("actor address %s present = %v, want %v", f.addr, found, f.want)
				}
				routes, err := netlink.RouteList(eth0, f.family)
				if err != nil {
					return err
				}
				var defaultRoute bool
				for _, r := range routes {
					defaultRoute = defaultRoute || (r.Gw != nil && (r.Dst == nil || r.Dst.IP.IsUnspecified()))
				}
				if defaultRoute != f.want {
					t.Errorf("family %d default route present = %v, want %v", f.family, defaultRoute, f.want)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("inspecting the actor's interface: %v", err)
		}
	}
}

func TestActorEgressIsFailClosedWithoutAtunnel(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	ctx := context.Background()

	for _, tc := range testFamilies {
		t.Run(tc.name, func(t *testing.T) {
			n, err := SetupSandboxNetwork(ctx, SandboxNetworkConfig{
				ActorUID: "33333333-3333-3333-3333-333333333333", Veth: true, Families: tc.families, EgressPort: testEgressPort,
			})
			if err != nil {
				t.Fatalf("SetupSandboxNetwork: %v", err)
			}
			t.Cleanup(func() { CleanupSandboxNetwork(n) })

			for _, destination := range []string{"93.184.216.34:443", "93.184.216.34:8080", "[2001:db8::1]:443"} {
				if err := netns.Do(ctx, n.RuntimeNetNS, func(context.Context) error {
					c, err := net.DialTimeout("tcp", destination, 3*time.Second)
					if err != nil {
						return err
					}
					c.Close()
					return nil
				}); err == nil {
					t.Errorf("the actor reached %s with no atunnel listening; egress is not fail-closed", destination)
				}
			}
		})
	}
}

func TestIngressCrossesThePairWhileEgressIsCaptured(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	for _, tc := range testFamilies {
		t.Run(tc.name, func(t *testing.T) { testIngressCrossesThePair(t, tc.families) })
	}
}

func testIngressCrossesThePair(t *testing.T, families Families) {
	ctx := context.Background()

	n, err := SetupSandboxNetwork(ctx, SandboxNetworkConfig{ActorUID: "44444444-4444-4444-4444-444444444444", Veth: true, Families: families, EgressPort: testEgressPort})
	if err != nil {
		t.Fatalf("SetupSandboxNetwork: %v", err)
	}
	t.Cleanup(func() { CleanupSandboxNetwork(n) })

	var app net.Listener
	if err := netns.Do(ctx, n.RuntimeNetNS, func(context.Context) error {
		l, e := net.Listen("tcp", net.JoinHostPort(families.ActorIP(), "80"))
		app = l
		return e
	}); err != nil {
		t.Fatalf("actor listen: %v", err)
	}
	defer app.Close()
	go func() {
		for {
			c, e := app.Accept()
			if e != nil {
				return
			}
			io.WriteString(c, "the-actor")
			c.Close()
		}
	}()

	dial := netns.Dialer(n.GatewayNetNS)
	c, err := dial(ctx, "tcp", net.JoinHostPort(families.ActorIP(), "80"))
	if err != nil {
		t.Fatalf("ingress dial: %v", err)
	}
	got, _ := io.ReadAll(c)
	c.Close()
	if string(got) != "the-actor" {
		t.Errorf("ingress reached %q, want %q", got, "the-actor")
	}

	// An unopened port must be refused, not redirected to atunnel.
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if c2, e := dial(cctx, "tcp", net.JoinHostPort(families.ActorIP(), "81")); e == nil {
		c2.Close()
		t.Error("a port with no listener was accepted, so ingress is not reaching the sandbox")
	}
}

// This tests namespace setup only; testing microVM egress requires a tap.
func TestSetupSandboxNetworkWithoutVeth(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	ctx := context.Background()

	n, err := SetupSandboxNetwork(ctx, SandboxNetworkConfig{
		ActorUID:   "77777777-7777-7777-7777-777777777777",
		Families:   dualStack,
		EgressPort: testEgressPort,
	})
	if err != nil {
		t.Fatalf("SetupSandboxNetwork: %v", err)
	}
	t.Cleanup(func() { CleanupSandboxNetwork(n) })

	if n.GatewayNetNS != n.RuntimeNetNS {
		t.Errorf("got a second namespace (%v vs %v); this shape needs one", n.GatewayNetNS, n.RuntimeNetNS)
	}

	// No veth was built, so nothing but lo is here until the tap arrives.
	if err := netns.Do(ctx, n.RuntimeNetNS, func(context.Context) error {
		links, err := netlink.LinkList()
		if err != nil {
			return err
		}
		for _, l := range links {
			if l.Attrs().Name != "lo" {
				t.Errorf("unexpected interface %q in the actor namespace", l.Attrs().Name)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("listing links: %v", err)
	}
}

func TestSetupSucceedsOverALeftoverNamespace(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	ctx := context.Background()
	const uid = "aaaaaaaa-0000-0000-0000-00000000000a"
	cfg := SandboxNetworkConfig{ActorUID: uid, Veth: true, Families: dualStack, EgressPort: testEgressPort}

	first, err := SetupSandboxNetwork(ctx, cfg)
	if err != nil {
		t.Fatalf("first SetupSandboxNetwork: %v", err)
	}
	// Simulate interrupted teardown by leaving the namespace names mounted.
	first.RuntimeNetNS.Close()
	if first.GatewayNetNS != first.RuntimeNetNS {
		first.GatewayNetNS.Close()
	}
	for _, name := range []string{nodepath.ActorNetNSName(uid), SandboxGatewayNetNSName(uid)} {
		if _, err := os.Stat("/var/run/netns/" + name); err != nil {
			t.Fatalf("expected leftover netns %s: %v", name, err)
		}
	}

	second, err := SetupSandboxNetwork(ctx, cfg)
	if err != nil {
		t.Fatalf("the actor is wedged by its own leftover namespace: %v", err)
	}
	t.Cleanup(func() { CleanupSandboxNetwork(second) })

	if err := netns.Do(ctx, second.RuntimeNetNS, func(context.Context) error {
		if _, err := netlink.LinkByName(ActorVethName); err != nil {
			return fmt.Errorf("actor interface missing after reuse: %w", err)
		}
		return nil
	}); err != nil {
		t.Error(err)
	}
}

// Check from the gateway: a successful UDP send from the actor does not prove delivery.
func TestActorUDPHasNowhereToGoBeyondTheNamespacePair(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	ctx := context.Background()

	n, err := SetupSandboxNetwork(ctx, SandboxNetworkConfig{
		ActorUID: "bbbbbbbb-0000-0000-0000-00000000000b", Veth: true, Families: dualStack, EgressPort: testEgressPort,
	})
	if err != nil {
		t.Fatalf("SetupSandboxNetwork: %v", err)
	}
	t.Cleanup(func() { CleanupSandboxNetwork(n) })

	for _, destination := range []string{"93.184.216.34:443", "93.184.216.34:53", "[2001:db8::1]:443", "[2001:db8::1]:53"} {
		if err := netns.Do(ctx, n.GatewayNetNS, func(context.Context) error {
			c, err := net.Dial("udp", destination)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Write([]byte("probe"))
			return err
		}); err == nil {
			t.Errorf("the atunnel namespace can reach %s over UDP; actor UDP could follow it out", destination)
		}
	}
}

func TestCleanupClosesEachDescriptorOnce(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	network, err := SetupSandboxNetwork(context.Background(), SandboxNetworkConfig{
		ActorUID:   "close-once",
		Families:   dualStack,
		EgressPort: 15001,
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.RuntimeNetNS != network.GatewayNetNS {
		t.Fatalf("expected one namespace without a veth, got %d and %d", network.RuntimeNetNS, network.GatewayNetNS)
	}

	// A second close of the same descriptor reports EBADF.
	if err := CleanupSandboxNetwork(network); err != nil {
		t.Fatalf("CleanupSandboxNetwork closed a descriptor twice: %v", err)
	}
}

// slowEgress holds its serving goroutine open until released, so a test can
// tell whether Close waits for serving to stop or merely closes the sockets.
type slowEgress struct{ release chan struct{} }

func (e *slowEgress) Bind(string) (func(context.Context, net.Listener) error, error) {
	return func(_ context.Context, l net.Listener) error {
		<-e.release
		return l.Close()
	}, nil
}

// Close's contract is that serving has stopped when it returns, not just that
// the sockets are shut: a caller tearing an actor down needs the worker's
// capacity back.
func TestSessionCloseWaitsForServingToStop(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	egress := &slowEgress{release: make(chan struct{})}
	session, err := ServeSandbox(context.Background(), SandboxNetworkConfig{
		ActorUID: "close-waits", Families: dualStack, EgressPort: testEgressPort,
	}, egress, nil)
	if err != nil {
		t.Fatal(err)
	}

	returned := make(chan error, 1)
	go func() { returned <- session.Close(context.Background()) }()
	select {
	case <-returned:
		t.Fatal("Close returned while the session was still serving")
	case <-time.After(250 * time.Millisecond):
	}

	close(egress.release)
	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("Close did not return after serving stopped")
	}
}

// A caller that cannot wait forever gets its deadline back as an error, and
// the namespaces are still removed -- leaving them would wedge the next
// activation of this actor.
func TestSessionCloseReportsAWaitItCouldNotFinish(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	egress := &slowEgress{release: make(chan struct{})}
	defer close(egress.release)
	session, err := ServeSandbox(context.Background(), SandboxNetworkConfig{
		ActorUID: "close-deadline", Families: dualStack, EgressPort: testEgressPort,
	}, egress, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := session.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v, want the deadline reported", err)
	}
	if _, err := netns.GetFromName(nodepath.ActorNetNSName("close-deadline")); err == nil {
		t.Error("the namespace survived a Close whose wait timed out")
	}
}

// Both sandbox shapes turn forwarding off and drop at the forward hook, whatever
// the node's own setting is.
func TestSandboxNamespacesDoNotForward(t *testing.T) {
	roottest.Require(t, "creates network namespaces")

	for _, shape := range []struct {
		name string
		uid  string
		veth bool
	}{
		{"veth", "cccccccc-0000-0000-0000-00000000000c", true},
		{"tap", "dddddddd-0000-0000-0000-00000000000d", false},
	} {
		for _, tc := range testFamilies {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) { testSandboxDoesNotForward(t, shape.uid, shape.veth, tc.families) })
		}
	}
}

func testSandboxDoesNotForward(t *testing.T, uid string, veth bool, families Families) {
	ctx := context.Background()
	n, err := SetupSandboxNetwork(ctx, SandboxNetworkConfig{ActorUID: uid, Veth: veth, Families: families, EgressPort: testEgressPort})
	if err != nil {
		t.Fatalf("SetupSandboxNetwork: %v", err)
	}
	t.Cleanup(func() { CleanupSandboxNetwork(n) })

	for _, ns := range []netns.Handle{n.RuntimeNetNS, n.GatewayNetNS} {
		if err := netns.Do(ctx, ns, func(context.Context) error {
			for _, key := range []string{
				"net/ipv4/conf/all/forwarding",
				"net/ipv4/conf/default/forwarding",
			} {
				b, err := os.ReadFile("/proc/sys/" + key)
				if err != nil {
					return err
				}
				if got := strings.TrimSpace(string(b)); got != "0" {
					t.Errorf("%s = %s in the sandbox namespace, want 0", key, got)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("reading forwarding sysctls: %v", err)
		}
	}

	c, err := nftables.New(nftables.WithNetNSFd(int(n.GatewayNetNS)))
	if err != nil {
		t.Fatal(err)
	}
	chains, err := c.ListChains()
	if err != nil {
		t.Fatal(err)
	}
	dropping := map[nftables.TableFamily]bool{}
	// The subnet accept, the rejected prefixes and the non-TCP
	// reject, plus the ICMPv6 accept for IPv6.
	wantDisallowedEgressRules := map[nftables.TableFamily]int{
		nftables.TableFamilyIPv4: 3,
		nftables.TableFamilyIPv6: 8,
	}
	redirecting := map[nftables.TableFamily]bool{}
	for _, ch := range chains {
		if ch.Hooknum != nil && *ch.Hooknum == *nftables.ChainHookForward &&
			ch.Policy != nil && *ch.Policy == nftables.ChainPolicyDrop {
			dropping[ch.Table.Family] = true
		}
		if ch.Type == nftables.ChainTypeNAT && ch.Name == "prerouting" {
			redirecting[ch.Table.Family] = true
		}
		if ch.Name == "disallowed-egress" {
			rules, err := c.GetRules(ch.Table, ch)
			if err != nil {
				t.Fatalf("getting disallowed-egress rules: %v", err)
			}
			if want := wantDisallowedEgressRules[ch.Table.Family]; len(rules) != want {
				t.Errorf("disallowed-egress in family %v has %d rules, want %d", ch.Table.Family, len(rules), want)
			}
			delete(wantDisallowedEgressRules, ch.Table.Family)
		}
	}
	for _, family := range []nftables.TableFamily{nftables.TableFamilyIPv4, nftables.TableFamilyIPv6} {
		if !dropping[family] {
			t.Errorf("no forward-hook chain with a drop policy for family %v", family)
		}
		if want := family == nftables.TableFamilyIPv4 && families.IPv4 || family == nftables.TableFamilyIPv6 && families.IPv6; redirecting[family] != want {
			t.Errorf("NAT prerouting chain for family %v present = %v, want %v", family, redirecting[family], want)
		}
	}
	for family := range wantDisallowedEgressRules {
		t.Errorf("no disallowed-egress chain found for family %v", family)
	}
}

// A new namespace copies conf/all and conf/default from the node, and a link
// copies conf/default when it is created. Both shapes end up with the pinned
// values on every link they add.
func TestSandboxNamespacesDoNotInheritNodeSettings(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	ctx := context.Background()

	// Settings the kernel reads from the link's own copy, alone or combined
	// with conf/all by OR or max, so a link seeded from the node's
	// conf/default would undo them. rp_filter=2 on a link wins over all=1.
	perLink := map[string]string{
		"forwarding":       "0",
		"accept_redirects": "0",
		"route_localnet":   "0",
		"accept_local":     "0",
		"rp_filter":        "1",
	}
	// IPv6 reads these only from the link's own copy.
	perLinkV6 := map[string]string{
		"disable_ipv6":     "0",
		"forwarding":       "0",
		"accept_ra":        "0",
		"accept_redirects": "0",
	}
	for _, tc := range []struct {
		name string
		uid  string
		veth bool
	}{
		{"veth", "a0a0a0a0-0000-0000-0000-00000000000a", true},
		{"tap", "b0b0b0b0-0000-0000-0000-00000000000b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := SetupSandboxNetwork(ctx, SandboxNetworkConfig{ActorUID: tc.uid, Veth: tc.veth, Families: dualStack, EgressPort: testEgressPort})
			if err != nil {
				t.Fatalf("SetupSandboxNetwork: %v", err)
			}
			t.Cleanup(func() { CleanupSandboxNetwork(n) })

			// A link added after setup, as the microVM tap is.
			if err := netns.Do(ctx, n.GatewayNetNS, func(context.Context) error {
				if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "late0"}, PeerName: "late1"}); err != nil {
					return err
				}
				for _, name := range []string{"late0", "late1"} {
					if err := linkUp(name); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("adding a late link: %v", err)
			}

			for _, ns := range []netns.Handle{n.RuntimeNetNS, n.GatewayNetNS} {
				if err := netns.Do(ctx, ns, func(context.Context) error {
					want := map[string]string{}
					for _, s := range sandboxSysctls {
						want[s.Key] = s.Value
					}
					links, err := netlink.LinkList()
					if err != nil {
						return err
					}
					for _, l := range links {
						name := l.Attrs().Name
						// lo predates the sysctls.
						if l.Attrs().Flags&net.FlagLoopback != 0 {
							continue
						}
						for key, value := range perLink {
							want["net/ipv4/conf/"+name+"/"+key] = value
						}
						for key, value := range perLinkV6 {
							want["net/ipv6/conf/"+name+"/"+key] = value
						}
					}
					for key, value := range want {
						b, err := os.ReadFile("/proc/sys/" + key)
						if errors.Is(err, fs.ErrNotExist) && strings.HasPrefix(key, "net/ipv6/") {
							continue
						}
						if err != nil {
							return err
						}
						if got := strings.TrimSpace(string(b)); got != value {
							t.Errorf("%s = %s, want %s", key, got, value)
						}
					}
					return nil
				}); err != nil {
					t.Fatalf("reading sysctls: %v", err)
				}
			}
		})
	}
}

// Adds a second link to the gateway namespace and checks that actor UDP is not
// routed onto it, even after the test turns forwarding back on. Deleting the
// forward chain at the end confirms the probe would see a routed packet.
func TestGatewayDoesNotRouteActorTrafficOverAStrayLink(t *testing.T) {
	roottest.Require(t, "creates network namespaces")
	ctx := context.Background()

	n, err := SetupSandboxNetwork(ctx, SandboxNetworkConfig{
		ActorUID: "eeeeeeee-0000-0000-0000-00000000000e", Veth: true, Families: dualStack, EgressPort: testEgressPort,
	})
	if err != nil {
		t.Fatalf("SetupSandboxNetwork: %v", err)
	}
	t.Cleanup(func() { CleanupSandboxNetwork(n) })

	// Delete disallowed-egress so its prerouting reject does not shadow the
	// forwarding sysctl and forward chain checks below.
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: "worker-actor"}
	c, err := nftables.New(nftables.WithNetNSFd(int(n.GatewayNetNS)))
	if err != nil {
		t.Fatal(err)
	}
	disallowedEgress := &nftables.Chain{Table: table, Name: "disallowed-egress"}
	c.FlushChain(disallowedEgress)
	c.DelChain(disallowedEgress)
	if err := c.Flush(); err != nil {
		_ = c.CloseLasting()
		t.Fatalf("removing disallowed-egress chain: %v", err)
	}
	_ = c.CloseLasting()

	probeNSName := fmt.Sprintf("actornet-forward-probe-%d", os.Getpid())
	probeNS, err := netns.CreateNamed(probeNSName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		probeNS.Close()
		_ = netns.RemoveNamed(probeNSName)
	})

	gwAddr, probeAddr := MustParseAddr("10.99.0.1/30"), MustParseAddr("10.99.0.2/30")
	if err := netns.Do(ctx, n.GatewayNetNS, func(context.Context) error {
		if err := netlink.LinkAdd(&netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{Name: "stray0"}, PeerName: "stray1",
			PeerNamespace: netlink.NsFd(int(probeNS)),
		}); err != nil {
			return err
		}
		l, err := netlink.LinkByName("stray0")
		if err != nil {
			return err
		}
		if err := netlink.AddrReplace(l, gwAddr); err != nil {
			return err
		}
		return netlink.LinkSetUp(l)
	}); err != nil {
		t.Fatalf("adding the stray link: %v", err)
	}

	var probe net.PacketConn
	if err := netns.Do(ctx, probeNS, func(context.Context) error {
		l, err := netlink.LinkByName("stray1")
		if err != nil {
			return err
		}
		if err := netlink.AddrReplace(l, probeAddr); err != nil {
			return err
		}
		if err := netlink.LinkSetUp(l); err != nil {
			return err
		}
		// A return route, so reverse-path filtering in the probe can't hide a forward.
		if err := netlink.RouteReplace(&netlink.Route{
			LinkIndex: l.Attrs().Index, Dst: MustParseAddr(ActorVethSubnet).IPNet, Gw: gwAddr.IP,
		}); err != nil {
			return err
		}
		probe, err = net.ListenPacket("udp4", "10.99.0.2:9999")
		return err
	}); err != nil {
		t.Fatalf("setting up the probe: %v", err)
	}
	t.Cleanup(func() { probe.Close() })

	arrives := func(wait time.Duration) bool {
		t.Helper()
		if err := netns.Do(ctx, n.RuntimeNetNS, func(context.Context) error {
			c, err := net.Dial("udp4", "10.99.0.2:9999")
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Write([]byte("routed"))
			return err
		}); err != nil {
			t.Fatalf("sending from the actor: %v", err)
		}
		_ = probe.SetReadDeadline(time.Now().Add(wait))
		buf := make([]byte, 16)
		_, _, err := probe.ReadFrom(buf)
		return err == nil
	}

	if arrives(200 * time.Millisecond) {
		t.Fatal("the gateway namespace routed actor traffic as set up")
	}

	if err := netns.Do(ctx, n.GatewayNetNS, func(context.Context) error {
		return os.WriteFile("/proc/sys/net/ipv4/conf/all/forwarding", []byte("1\n"), 0o644)
	}); err != nil {
		t.Fatalf("re-enabling forwarding: %v", err)
	}
	if arrives(200 * time.Millisecond) {
		t.Fatal("the gateway namespace routed actor traffic once forwarding was re-enabled; the forward chain is not dropping")
	}

	c, err = nftables.New(nftables.WithNetNSFd(int(n.GatewayNetNS)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseLasting() }()
	c.DelChain(&nftables.Chain{Name: "forward", Table: table})
	if err := c.Flush(); err != nil {
		t.Fatalf("removing the forward chain: %v", err)
	}
	if !arrives(5 * time.Second) {
		t.Fatal("the probe saw nothing even with forwarding on and no forward chain; this test cannot detect routing")
	}
}
