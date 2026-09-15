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
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// InstallGatewayNftablesRules installs TCP capture and denies all IP forwarding
// in the current sandbox gateway namespace. Proxy sockets use INPUT/OUTPUT,
// so they do not require forwarding. Routing to egress endpoints is managed
// separately.
// REDIRECT rewrites the destination to a local listener; atunnel recovers the
// requested TCP destination with SO_ORIGINAL_DST. Only sandbox arrivals hit
// these PREROUTING rules. Proxy-created sockets use OUTPUT, so they cannot be
// recaptured and do not need a bypass mark in this topology.
// Updates replace this table atomically, including its fail-closed policy.
func InstallGatewayNftablesRules(egressPort uint16) error {
	if egressPort == 0 {
		return fmt.Errorf("gateway capture requires a nonzero TCP port")
	}
	c := &nftables.Conn{}
	const name = "ateom_gateway"
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return fmt.Errorf("listing gateway nftables tables: %w", err)
	}
	for _, table := range tables {
		if table.Name == name {
			c.DelTable(table)
		}
	}
	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: name})
	prerouting := c.AddChain(&nftables.Chain{Name: "prerouting", Table: table,
		Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest})
	// Every capture rule matches the sandbox-facing interface and IPv4 source.
	fromSandbox := append([]expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{byte(nftables.TableFamilyIPv4)}},
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(HostVethName), 0)},
	}, ipSourceEqual(ActorVethIP)...)
	// DNS precedes general TCP interception; zero matchPort means all ports.
	for _, capture := range []struct {
		protocol                byte
		matchPort, redirectPort uint16
	}{{unix.IPPROTO_TCP, 53, 53}, {unix.IPPROTO_UDP, 53, 53}, {unix.IPPROTO_TCP, 0, egressPort}} {
		expressions := append(slices.Clone(fromSandbox), l4ProtocolEqual(capture.protocol)...)
		if capture.matchPort != 0 {
			expressions = append(expressions,
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binary.BigEndian.AppendUint16(nil, capture.matchPort)},
			)
		}
		expressions = append(expressions,
			&expr.Immediate{Register: 1, Data: binary.BigEndian.AppendUint16(nil, capture.redirectPort)},
			&expr.Redir{RegisterProtoMin: 1},
		)
		c.AddRule(&nftables.Rule{Table: table, Chain: prerouting, Exprs: expressions})
	}
	// The inet family drops uncaptured IPv4 and IPv6 alike, including UDP other
	// than DNS and traffic with a forged source. This is required even without a
	// default route: atenet /32 routes must not become a direct sandbox exit.
	drop := nftables.ChainPolicyDrop
	c.AddChain(&nftables.Chain{Name: "forward", Table: table,
		Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &drop})
	if err := c.Flush(); err != nil {
		return fmt.Errorf("installing gateway nftables rules: %w", err)
	}
	return nil
}
