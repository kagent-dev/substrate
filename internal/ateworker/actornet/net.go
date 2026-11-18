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

// Package actornet provides shared networking configuration logic for Substrate runtime agents.
package actornet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	ActorVethName    = "eth0"
	ActorVethGateway = "169.254.17.1"
	ActorVethIP      = "169.254.17.2"

	// hostVethLocalAddress is the gateway interface's IP address and prefix length.
	hostVethLocalAddress = "169.254.17.1/30"
	// actorVethLocalAddress is the actor interface's IP address and prefix length.
	actorVethLocalAddress = "169.254.17.2/30"

	// ActorVethSubnet is the point-to-point /30 the actor veth lives on.
	ActorVethSubnet = "169.254.17.0/30"

	// The IPv6 counterparts, a /126 in a fixed ULA prefix. Link-local alone
	// would leave the actor no source address for other destinations. A ULA
	// has global scope and 169.254.0.0/16 link-local scope, so RFC 6724
	// address selection prefers IPv6 for names that have both.
	ActorVethGatewayV6      = "fd00:a7e:17::1"
	ActorVethIPv6           = "fd00:a7e:17::2"
	hostVethLocalAddressV6  = "fd00:a7e:17::1/126"
	actorVethLocalAddressV6 = "fd00:a7e:17::2/126"
	ActorVethSubnetV6       = "fd00:a7e:17::/126"
)

var (
	HostVethAddr  = MustParseAddr(hostVethLocalAddress)
	ActorVethAddr = MustParseAddr(actorVethLocalAddress)
	ActorVethGwIP = MustParseIP(ActorVethGateway)

	HostVethAddrV6  = mustParseAddrNoDAD(hostVethLocalAddressV6)
	actorVethAddrV6 = mustParseAddrNoDAD(actorVethLocalAddressV6)
	actorVethGwIPv6 = MustParseIP(ActorVethGatewayV6)
)

// Families are the IP families a sandbox network carries. At least one is
// set.
type Families struct {
	IPv4 bool
	IPv6 bool
}

// PodFamilies returns the families of the worker pod's IPs, so a sandbox
// carries the families its cluster does.
func PodFamilies(podIPs []string) (Families, error) {
	var f Families
	for _, s := range podIPs {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return Families{}, fmt.Errorf("actornet: parsing pod IP %q: %w", s, err)
		}
		if ip.Unmap().Is4() {
			f.IPv4 = true
		} else {
			f.IPv6 = true
		}
	}
	if err := f.validate(); err != nil {
		return Families{}, fmt.Errorf("actornet: pod IPs %q: %w", podIPs, err)
	}
	return f, nil
}

func (f Families) validate() error {
	if !f.IPv4 && !f.IPv6 {
		return errors.New("no IP family")
	}
	return nil
}

// ActorIP is the actor's address for ingress and wakeup probes: IPv4 where
// the sandbox has it, IPv6 otherwise.
func (f Families) ActorIP() string {
	if f.IPv4 {
		return ActorVethIP
	}
	return ActorVethIPv6
}

// HTTPUpstream is the in-sandbox HTTP endpoint atunnel proxies actor ingress
// to.
func (f Families) HTTPUpstream() string {
	return "http://" + net.JoinHostPort(f.ActorIP(), "80")
}

// actorIPs are the actor's addresses, IPv4 first.
func (f Families) actorIPs() []string {
	return f.pick(ActorVethIP, ActorVethIPv6)
}

// gatewayIPs are the gateway's addresses, where the DNS relay answers, IPv4
// first.
func (f Families) gatewayIPs() []string {
	return f.pick(ActorVethGateway, ActorVethGatewayV6)
}

// GatewayAddrs are the gateway interface's addresses.
func (f Families) GatewayAddrs() []*netlink.Addr {
	var out []*netlink.Addr
	if f.IPv4 {
		out = append(out, HostVethAddr)
	}
	if f.IPv6 {
		out = append(out, HostVethAddrV6)
	}
	return out
}

func (f Families) pick(v4, v6 string) []string {
	var out []string
	if f.IPv4 {
		out = append(out, v4)
	}
	if f.IPv6 {
		out = append(out, v6)
	}
	return out
}

// mustParseAddrNoDAD skips duplicate address detection, which would hold the
// address tentative for a second on a link where nothing else can claim it.
func mustParseAddrNoDAD(cidr string) *netlink.Addr {
	a := MustParseAddr(cidr)
	a.Flags = unix.IFA_F_NODAD
	return a
}

// MustParseAddr parses a CIDR string into a netlink.Addr, panicking on error.
func MustParseAddr(cidr string) *netlink.Addr {
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		panic(fmt.Sprintf("parsing constant CIDR %q: %v", cidr, err))
	}
	return a
}

// MustParseIP parses an IP string into a net.IP, panicking on error. IPv4
// addresses come back in their 4-byte form.
func MustParseIP(s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		panic(fmt.Sprintf("parsing constant IP %q", s))
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4
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

func l4ProtocolNotEqual(proto byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 1,
			Data:     []byte{proto},
		},
	}
}
