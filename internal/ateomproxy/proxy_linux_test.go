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

package ateomproxy_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/ateomnet"
	"github.com/agent-substrate/substrate/internal/ateomproxy"
	"github.com/agent-substrate/substrate/internal/ateomproxy/proxytest"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/roottest"
)

func TestProxySessionRenewal(t *testing.T) {
	roottest.Require(t, "proxy session lifecycle across certificate expiry")
	if !proxytest.Enter(t) {
		return
	}
	const lifetime = 2 * time.Second
	bundle, trust, gateway := proxytest.Egress(t, lifetime)
	p, err := ateomproxy.New(t.Context(), proxytest.Config("session-worker", false, bundle, trust))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	actor := resources.ActorAttribution{Ref: resources.ActorRef{Atespace: "test", Name: "actor"}, UID: "actor-uid"}
	session, err := p.Prepare(t.Context(), actor, gateway)
	if err != nil {
		t.Fatal(err)
	}
	checkEgress := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		conn, err := ateomnet.DialTCP(ctx, p.Net.Runtime, netip.MustParseAddrPort("198.51.100.10:18080"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(conn, "probe"); err != nil {
			t.Fatal(err)
		}
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(conn)
		if err != nil || string(body) != actor.UID+"/probe" {
			t.Fatalf("egress = %q, %v", body, err)
		}
	}
	checkEgress()
	proxytest.Request(t, p.Ingress, "actor", "/", 421)
	if err := p.Deactivate(t.Context()); err != nil {
		t.Fatal(err)
	}
	proxytest.CheckEgressInactive(t, p)
	// The broker's first certificate expires while renewal is paused.
	time.Sleep(lifetime)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := session.Resume(ctx); err != nil {
		t.Fatalf("resume after expiry with canceled caller: %v", err)
	}
	checkEgress()
	proxytest.Request(t, p.Ingress, "actor", "/", 502)
	if err := p.Reset(t.Context()); err != nil {
		t.Fatal(err)
	}
	proxytest.CheckEgressInactive(t, p)
	proxytest.Request(t, p.Ingress, "actor", "/", 421)
}

func TestProxyConstructorRollback(t *testing.T) {
	roottest.Require(t, "proxy listener and namespace cleanup after construction failure")
	if !proxytest.Enter(t) {
		return
	}
	bundle, trust, _ := proxytest.Egress(t)
	config := proxytest.Config("rollback-worker", false, bundle, trust)
	ingress, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ingress.Close()
	config.IngressAddress = ingress.Addr().String()
	collision, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer collision.Close()
	config.ConnectAddress = collision.Addr().String()
	if err := ingress.Close(); err != nil {
		t.Fatal(err)
	}
	if p, err := ateomproxy.New(t.Context(), config); err == nil {
		_ = p.Close()
		t.Fatal("constructor accepted occupied CONNECT address")
	}
	if err := collision.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := ateomproxy.New(t.Context(), config)
	if err != nil {
		t.Fatalf("retry with same namespaces and listener addresses: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
