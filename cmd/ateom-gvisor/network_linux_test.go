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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/actorlog"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/ateomproxy"
	"github.com/agent-substrate/substrate/internal/ateomproxy/proxytest"
	"github.com/agent-substrate/substrate/internal/ocispec"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/internal/roottest"
	"google.golang.org/protobuf/proto"
)

func TestProductionWorkerGVisor(t *testing.T) {
	roottest.Require(t, "production runtime lifecycle")
	if !proxytest.Enter(t, "ATE_TEST_RUNSC", "ATE_TEST_NETWORK_PROBE") {
		return
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go reaper.Run(ctx)
	// The isolated test has no delegated cgroup. Production still uses shaped
	// cgroups; this wrapper only disables host cgroups for the fixture.
	wrapper := filepath.Join(t.TempDir(), "runsc")
	script := "#!/bin/sh\nexec " + os.Getenv("ATE_TEST_RUNSC") + " --ignore-cgroups --platform=systrap \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	const initialLifetime = 2 * time.Second
	bundle, trust, gateway := proxytest.Egress(t, initialLifetime)
	workerUID := "production-gvisor"
	proxy, err := ateomproxy.New(ctx, proxytest.Config(workerUID, false, bundle, trust))
	if err != nil {
		t.Fatal(err)
	}
	shared := proxy.Ingress
	w := NewService(proxy, actorlog.NewActorLogger(io.Discard, false))
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	const name, uid = "a", "a-gvisor"
	for _, container := range []string{ocispec.PauseContainer, "app"} {
		proxytest.Bundle(t, workerUID, uid, container, os.Getenv("ATE_TEST_NETWORK_PROBE"))
	}
	a := &ateompb.RunWorkloadRequest{Atespace: "test", ActorName: name, ActorUid: uid, RunscPath: wrapper, Spec: proxytest.Spec(), EgressGateway: gateway, CpuMilli: 1000, MemoryBytes: 256 << 20}
	t.Cleanup(func() {
		_, _ = w.TerminateWorkload(context.Background(), &ateompb.TerminateWorkloadRequest{Atespace: a.Atespace, ActorName: a.ActorName, ActorUid: a.ActorUid, RunscPath: a.RunscPath, Spec: a.Spec})
	})
	bad := proto.Clone(a).(*ateompb.RunWorkloadRequest)
	bad.EgressGateway = &ateompb.EgressGateway{Address: "missing-port"}
	if _, err := w.RunWorkload(ctx, bad); err == nil {
		t.Fatal("invalid gateway accepted")
	}

	// Fail after credentials are active, then verify the proxy rejects DNS.
	failed := proto.Clone(a).(*ateompb.RunWorkloadRequest)
	failed.ActorUid = "failed-" + uid
	for _, container := range []string{ocispec.PauseContainer, "app"} {
		proxytest.Bundle(t, workerUID, failed.ActorUid, container, os.Getenv("ATE_TEST_NETWORK_PROBE"))
	}
	failed.RunscPath = filepath.Join(t.TempDir(), "missing-runsc")
	if _, err := w.RunWorkload(ctx, failed); err == nil || !strings.Contains(err.Error(), failed.RunscPath) {
		t.Fatalf("expected runtime startup failure, got %v", err)
	}
	proxytest.CheckEgressInactive(t, proxy)
	// A missing full snapshot fails after the pause container is created.
	_, err = w.RestoreWorkload(ctx, &ateompb.RestoreWorkloadRequest{Atespace: "test", ActorName: name, ActorUid: failed.ActorUid, RunscPath: wrapper, Spec: a.Spec, EgressGateway: gateway, Scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL, CpuMilli: 1000, MemoryBytes: 256 << 20})
	if err == nil || !strings.Contains(err.Error(), "while restoring pause container") {
		t.Fatalf("expected missing snapshot failure, got %v", err)
	}
	proxytest.CheckEgressInactive(t, proxy)

	if _, err := w.RunWorkload(ctx, a); err != nil {
		t.Fatal(err)
	}
	proxytest.CheckCount(t, shared, name, 1)
	if got := proxytest.Request(t, shared, name, "/egress", 200); got != uid+"/runtime" {
		t.Fatalf("wrong egress identity: %s", got)
	}
	if got := proxytest.Request(t, shared, name, "/dns", 200); got != "203.0.113.9" {
		t.Fatalf("DNS: %s", got)
	}
	if mtu := proxytest.Request(t, shared, name, "/mtu", 200); mtu != "1400" {
		t.Fatalf("MTU %s", mtu)
	}
	// The original credential has expired; recovery must use its renewal.
	time.Sleep(initialLifetime)
	if _, err := w.CheckpointWorkload(ctx, &ateompb.CheckpointWorkloadRequest{Atespace: "test", ActorName: "a", ActorUid: a.ActorUid, Spec: a.Spec, RunscPath: a.RunscPath}); err == nil {
		t.Fatal("invalid checkpoint accepted")
	}
	proxytest.Request(t, shared, "a", "/mtu", 200)
	if got := proxytest.Request(t, shared, "a", "/egress", 200); got != a.ActorUid+"/runtime" {
		t.Fatalf("egress after failed checkpoint and renewal: %s", got)
	}
	cp, err := w.CheckpointWorkload(ctx, &ateompb.CheckpointWorkloadRequest{Atespace: "test", ActorName: "a", ActorUid: a.ActorUid, RunscPath: wrapper, Spec: a.Spec, Scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL})
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.SnapshotFiles) == 0 {
		t.Fatal("empty snapshot")
	}
	proxytest.Request(t, shared, "a", "/count", 421)
	if err := os.Rename(ateompath.CheckpointStateDir(a.ActorUid), ateompath.RestoreStateDir(a.ActorUid)); err != nil {
		t.Fatal(err)
	}
	_, err = w.RestoreWorkload(ctx, &ateompb.RestoreWorkloadRequest{Atespace: "test", ActorName: "a", ActorUid: a.ActorUid, RunscPath: wrapper, Spec: a.Spec, EgressGateway: gateway, Scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL, CpuMilli: 1000, MemoryBytes: 256 << 20})
	if err != nil {
		t.Fatalf("restore from %s: %v", ateompath.CheckpointStateDir(a.ActorUid), err)
	}
	proxytest.CheckCount(t, shared, "a", 2)
	if got := proxytest.Request(t, shared, "a", "/egress", 200); got != a.ActorUid+"/runtime" {
		t.Fatalf("restored egress: %s", got)
	}
	if _, err := w.TerminateWorkload(ctx, &ateompb.TerminateWorkloadRequest{Atespace: "test", ActorName: "a", ActorUid: a.ActorUid, RunscPath: wrapper, Spec: a.Spec}); err != nil {
		t.Fatal(err)
	}
	b := proto.Clone(a).(*ateompb.RunWorkloadRequest)
	b.ActorName, b.ActorUid = "b", "b-runtime"
	for _, container := range []string{ocispec.PauseContainer, "app"} {
		proxytest.Bundle(t, workerUID, b.ActorUid, container, os.Getenv("ATE_TEST_NETWORK_PROBE"))
	}
	t.Cleanup(func() {
		_, _ = w.TerminateWorkload(context.Background(), &ateompb.TerminateWorkloadRequest{Atespace: b.Atespace, ActorName: b.ActorName, ActorUid: b.ActorUid, RunscPath: b.RunscPath, Spec: b.Spec})
	})
	if _, err := w.RunWorkload(ctx, b); err != nil {
		t.Fatalf("subsequent actor: %v", err)
	}
	proxytest.CheckCount(t, shared, "b", 1)
	proxytest.Request(t, shared, "a", "/count", 421)
	if got := proxytest.Request(t, shared, "b", "/egress", 200); got != b.ActorUid+"/runtime" {
		t.Fatalf("subsequent actor egress: %s", got)
	}
}
