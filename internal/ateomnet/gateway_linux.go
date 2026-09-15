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
	"fmt"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// InstallGatewayNftablesRules installs TCP capture and denies all IP forwarding
// in the current sandbox gateway namespace. Proxy sockets use INPUT/OUTPUT,
// so they do not require forwarding. Call only in a dedicated gateway without
// the legacy actor NAT table; routing to egress endpoints is managed separately.
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
	rule := ActorEgressRedirectRule(table, prerouting, egressPort)
	// The source-address payload match is IPv4-specific. Scope it explicitly
	// even though the forward policy covers both IPv4 and IPv6.
	rule.Exprs = append([]expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{byte(nftables.TableFamilyIPv4)}},
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(HostVethName), 0)},
	}, rule.Exprs...)
	c.AddRule(rule)
	drop := nftables.ChainPolicyDrop
	c.AddChain(&nftables.Chain{Name: "forward", Table: table,
		Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &drop})
	if err := c.Flush(); err != nil {
		return fmt.Errorf("installing gateway nftables rules: %w", err)
	}
	return nil
}
