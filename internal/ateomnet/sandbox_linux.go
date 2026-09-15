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

package ateomnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Sandbox owns one gateway and, for gVisor, a separate runtime namespace.
// The links form one of these paths:
//
//	worker eth0 <-- transit veth --> Gateway <-- sandbox veth --> gVisor Runtime
//	worker eth0 <-- transit veth --> Gateway TAP <--> MicroVM guest kernel
//
// gVisor takes over the runtime interfaces with its userspace netstack, so the
// proxy's Linux sockets live across the veth in Gateway. A VM already has its
// own guest kernel; its host TAP goes directly in Gateway, with no Runtime NS.
// In either case sandbox packets terminate at the proxy, which opens a separate
// authenticated connection through the transit link to atenet.
//
// Its gateway never has a default route. Only local proxy sockets can use the
// explicit endpoint routes; forwarded sandbox packets are always dropped.
type Sandbox struct {
	Gateway, Runtime         netns.NsHandle
	MTU                      int
	workerLink               string
	gatewayName, runtimeName string
	workerIP                 net.IP
	table                    *nftables.Table
	mu                       sync.RWMutex
	closed                   bool
}

// NewSandbox must run in the worker namespace. Each transit /30 is reserved by
// its worker veth, so allocation also accounts for surviving kernel state.
func NewSandbox(ctx context.Context, workerUID string, microVM bool) (_ *Sandbox, retErr error) {
	n := &Sandbox{Gateway: -1, Runtime: -1}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, n.Close())
		}
	}()
	runtimeName := ateompath.AteomNetNSName(workerUID)
	gatewayName := runtimeName + "-gw"
	// Refuse stale named namespaces: never attach a new identity to old sockets.
	for _, name := range []string{runtimeName, gatewayName} {
		if _, err := os.Stat("/run/netns/" + name); err == nil {
			return nil, fmt.Errorf("namespace %s already exists; replace the stale worker", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	n.runtimeName, n.gatewayName = runtimeName, gatewayName
	var err error
	n.Gateway, err = CreateNetNSWithoutSwitching(n.gatewayName)
	if err != nil {
		n.gatewayName = ""
		n.runtimeName = ""
		return nil, err
	}
	if !microVM {
		n.Runtime, err = CreateNetNSWithoutSwitching(n.runtimeName)
		if err != nil {
			n.runtimeName = ""
			return nil, err
		}
	} else {
		n.runtimeName = ""
	}
	eth, err := netlink.LinkByName("eth0")
	if err != nil {
		return nil, fmt.Errorf("worker uplink: %w", err)
	}
	n.MTU = eth.Attrs().MTU
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	occupied := make(map[string]bool, len(links))
	for _, link := range links {
		occupied[link.Attrs().Name] = true
	}
	var slot int
	for slot = 0; slot < 8192; slot++ {
		name := fmt.Sprintf("atw%d", slot)
		if occupied[name] {
			continue
		}
		// 169.254.32.0 through 169.254.159.255 excludes guest and cloud metadata addresses.
		base := 0xA9FE2000 + uint32(slot)*4
		n.workerIP = net.IPv4(byte(base>>24), byte(base>>16), byte(base>>8), byte(base+1)).To4()
		peerIP := net.IPv4(byte(base>>24), byte(base>>16), byte(base>>8), byte(base+2)).To4()
		conflict := false
		candidate := &net.IPNet{IP: n.workerIP.Mask(net.CIDRMask(30, 32)), Mask: net.CIDRMask(30, 32)}
		for _, route := range routes {
			if route.Dst == nil {
				continue
			}
			bits, _ := route.Dst.Mask.Size()
			if bits > 0 && (route.Dst.Contains(n.workerIP) || candidate.Contains(route.Dst.IP)) {
				conflict = true
				break
			}
		}
		if conflict {
			continue
		}
		v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: n.MTU}, PeerName: "up0", PeerNamespace: netlink.NsFd(n.Gateway), PeerMTU: uint32(n.MTU)}
		if err := netlink.LinkAdd(v); err != nil {
			return nil, fmt.Errorf("creating transit: %w", err)
		}
		n.workerLink = name
		if err := addressLink(v, n.workerIP.String()+"/30"); err != nil {
			return nil, err
		}
		if err := NetNSDo(ctx, n.Gateway, func(context.Context) error {
			lo, err := netlink.LinkByName("lo")
			if err != nil {
				return err
			}
			if err = netlink.LinkSetUp(lo); err != nil {
				return err
			}
			up, err := netlink.LinkByName("up0")
			if err != nil {
				return err
			}
			return addressLink(up, peerIP.String()+"/30")
		}); err != nil {
			return nil, err
		}
		// Only proxy-originated traffic reaches this link. Masquerading its transit
		// source in the worker lets replies return without teaching the pod network
		// about our private /30. Gateway's FORWARD drop blocks sandbox bypass.
		c := &nftables.Conn{}
		n.table = c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: name})
		chain := c.AddChain(&nftables.Chain{Name: "postrouting", Table: n.table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource})
		c.AddRule(&nftables.Rule{Table: n.table, Chain: chain, Exprs: append(ipSourceEqual(peerIP.String()), &expr.Masq{})})
		if err := c.Flush(); err != nil {
			return nil, err
		}
		break
	}
	if slot == 8192 {
		return nil, fmt.Errorf("worker transit addresses exhausted")
	}
	if err := EnableIPv4Forwarding(); err != nil {
		return nil, err
	}
	return n, nil
}

func addressLink(l netlink.Link, cidr string) error {
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		return err
	}
	if err = netlink.AddrReplace(l, a); err != nil {
		return err
	}
	return netlink.LinkSetUp(l)
}

// Setup installs fail-closed capture before admitting sandbox traffic. On a VM,
// the TAP is added separately after vm.create; on gVisor this creates the veth.
func (n *Sandbox) Setup(ctx context.Context, port uint16) error {
	return NetNSDo(ctx, n.Gateway, func(context.Context) (retErr error) {
		if err := InstallGatewayNftablesRules(port); err != nil {
			return err
		}
		if n.Runtime <= 0 {
			return nil
		}
		if _, err := netlink.LinkByName(HostVethName); err == nil {
			return nil
		}
		v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: HostVethName, MTU: n.MTU}, PeerName: ActorVethName, PeerNamespace: netlink.NsFd(n.Runtime), PeerMTU: uint32(n.MTU)}
		if err := netlink.LinkAdd(v); err != nil {
			return err
		}
		defer func() {
			if retErr != nil {
				retErr = errors.Join(retErr, netlink.LinkDel(v))
			}
		}()
		if err := addressLink(v, HostVethCIDR); err != nil {
			return err
		}
		return NetNSDo(ctx, n.Runtime, ConfigureActorVeth)
	})
}

// Reset removes only the sandbox-facing device, preserving the proxy listeners
// and transit link during a cold-boot retry. The worker releases namespaces.
func (n *Sandbox) Reset(ctx context.Context) error {
	return NetNSDo(ctx, n.Gateway, func(context.Context) error {
		l, err := netlink.LinkByName(HostVethName)
		if _, ok := errors.AsType[netlink.LinkNotFoundError](err); ok {
			return nil
		}
		if err != nil {
			return err
		}
		return netlink.LinkDel(l)
	})
}

// DialContext connects to an application in this sandbox, without adding routes.
// Ingress and readiness both dial the actor IP from Gateway; loopback here would
// address the gateway itself, not the gVisor netstack or the VM guest.
func (n *Sandbox) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("unsupported network %q", network)
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, err
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.closed {
		return nil, net.ErrClosed
	}
	return DialTCP(ctx, n.Gateway, ap)
}

// DialEndpoint resolves trusted control-plane endpoints on the worker, installs
// /32 routes, and creates the connection in the gateway. Workloads cannot call it.
// This dials the atenet tunnel endpoint, not the workload's requested destination;
// that destination travels inside CONNECT. Resolution uses the worker namespace
// so establishing the tunnel never depends on the DNS service it carries.
func (n *Sandbox) DialEndpoint(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("unsupported network %q", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return nil, fmt.Errorf("invalid endpoint port %q", port)
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.closed {
		return nil, net.ErrClosed
	}
	for _, ip := range ips {
		if ip.IsLinkLocalUnicast() || ip.IsLoopback() || ip.IsUnspecified() {
			return nil, fmt.Errorf("atenet endpoint must be routable from the worker: %s", ip)
		}
		err = NetNSDo(ctx, n.Gateway, func(context.Context) error {
			up, e := netlink.LinkByName("up0")
			if e != nil {
				return e
			}
			return netlink.RouteReplace(&netlink.Route{LinkIndex: up.Attrs().Index, Gw: n.workerIP, Dst: &net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(32, 32)}})
		})
		if err != nil {
			return nil, err
		}
		var conn *net.TCPConn
		conn, err = DialTCP(ctx, n.Gateway, netip.AddrPortFrom(ip.Unmap(), uint16(p)))
		if err == nil {
			return conn, nil
		}
	}
	if err == nil {
		err = fmt.Errorf("endpoint %q has no IPv4 address", host)
	}
	return nil, err
}

// Close releases only this sandbox's kernel state. Call after stopping its
// proxy handlers and runtime; keeping a namespace FD cannot authorize reuse.
func (n *Sandbox) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	var errs []error
	if n.workerLink != "" {
		if l, err := netlink.LinkByName(n.workerLink); err == nil {
			errs = append(errs, netlink.LinkDel(l))
		} else {
			errs = append(errs, err)
		}
	}
	if n.table != nil {
		c := &nftables.Conn{}
		c.DelTable(n.table)
		errs = append(errs, c.Flush())
	}
	if n.Runtime > 0 {
		errs = append(errs, n.Runtime.Close())
	}
	if n.Gateway > 0 {
		errs = append(errs, n.Gateway.Close())
	}
	if n.runtimeName != "" {
		errs = append(errs, netns.DeleteNamed(n.runtimeName))
	}
	if n.gatewayName != "" {
		errs = append(errs, netns.DeleteNamed(n.gatewayName))
	}
	return errors.Join(errs...)
}
