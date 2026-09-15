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
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/agent-substrate/substrate/internal/actorlog"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/ateomproxy"
	"github.com/agent-substrate/substrate/internal/ateomproxy/proxytest"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/internal/roottest"
	"github.com/vishvananda/netns"
	"google.golang.org/protobuf/proto"
)

func TestProductionWorkerMicroVM(t *testing.T) {
	roottest.Require(t, "production runtime lifecycle")
	if !proxytest.Enter(t, "ATE_TEST_MICROVM_ASSETS", "ATE_TEST_NETWORK_PROBE") {
		return
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	assets := os.Getenv("ATE_TEST_MICROVM_ASSETS")
	paths := map[string]string{assetCH: filepath.Join(assets, "cloud-hypervisor"), assetKernel: filepath.Join(assets, "vmlinux"), assetImage: filepath.Join(assets, "rootfs.img"), assetConfig: filepath.Join(assets, "configuration-clh.toml"), assetVirtiofsd: filepath.Join(assets, "virtiofsd")}
	const initialLifetime = 2 * time.Second
	bundle, trust, gateway := proxytest.Egress(t, initialLifetime)
	workerUID := "production-microvm"
	proxy, err := ateomproxy.New(ctx, proxytest.Config(workerUID, true, bundle, trust))
	if err != nil {
		t.Fatal(err)
	}
	shared := proxy.Ingress
	w := NewService(workerUID, paths[assetCH], paths[assetConfig], false, 128, proxy, actorlog.NewActorLogger(io.Discard, false))
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	const name, uid = "a", "a-microvm"
	proxytest.Bundle(t, workerUID, uid, "app", os.Getenv("ATE_TEST_NETWORK_PROBE"))
	a := &ateompb.RunWorkloadRequest{Atespace: "test", ActorName: name, ActorUid: uid, Spec: proxytest.Spec(), EgressGateway: gateway, CpuMilli: 1000, MemoryBytes: 512 << 20, RuntimeAssetPaths: paths}
	t.Cleanup(func() {
		_, _ = w.TerminateWorkload(context.Background(), &ateompb.TerminateWorkloadRequest{Atespace: a.Atespace, ActorName: a.ActorName, ActorUid: a.ActorUid, RunscPath: a.RunscPath, Spec: a.Spec})
	})
	bad := proto.Clone(a).(*ateompb.RunWorkloadRequest)
	bad.EgressGateway = &ateompb.EgressGateway{Address: "missing-port"}
	if _, err := w.RunWorkload(ctx, bad); err == nil {
		t.Fatal("invalid gateway accepted")
	}

	// Invalid configuration fails after credentials are active, without booting.
	failed := proto.Clone(a).(*ateompb.RunWorkloadRequest)
	failed.ActorUid = "failed-" + uid
	invalidConfig := filepath.Join(t.TempDir(), "invalid.toml")
	if err := os.WriteFile(invalidConfig, []byte("[invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	failed.RuntimeAssetPaths[assetConfig] = invalidConfig
	if _, err := w.RunWorkload(ctx, failed); err == nil || !strings.Contains(err.Error(), "while parsing kata config") {
		t.Fatalf("expected runtime configuration failure, got %v", err)
	}
	proxytest.CheckEgressInactive(t, proxy)

	if _, err := w.RunWorkload(ctx, a); err != nil {
		t.Fatal(err)
	}
	checkVMMNetNS(t, w, uid)
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
	// A mounted checkpoint directory fails removal after vm.pause. Recovery
	// must resume the VM before publishing its ingress again.
	checkpointDir := ateompath.CheckpointStateDir(a.ActorUid)
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(checkpointDir, checkpointDir, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(checkpointDir, unix.MNT_DETACH) })
	_, checkpointErr := w.CheckpointWorkload(ctx, &ateompb.CheckpointWorkloadRequest{Atespace: "test", ActorName: "a", ActorUid: a.ActorUid, Spec: a.Spec, Scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL})
	if err := unix.Unmount(checkpointDir, 0); err != nil {
		t.Fatal(err)
	}
	if checkpointErr == nil {
		t.Fatal("checkpoint into a mounted directory succeeded")
	}
	proxytest.Request(t, shared, "a", "/mtu", 200)
	cp, err := w.CheckpointWorkload(ctx, &ateompb.CheckpointWorkloadRequest{Atespace: "test", ActorName: "a", ActorUid: a.ActorUid, Spec: a.Spec, Scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL})
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

	// Keep valid snapshot metadata, but corrupt the network device type so the
	// restore fails after preparing authenticated egress.
	snapshotConfig := filepath.Join(ateompath.RestoreStateDir(a.ActorUid), "config.json")
	originalConfig, err := os.ReadFile(snapshotConfig)
	if err != nil {
		t.Fatal(err)
	}
	var invalidSnapshot map[string]any
	if err := json.Unmarshal(originalConfig, &invalidSnapshot); err != nil {
		t.Fatal(err)
	}
	invalidSnapshot["net"] = "invalid"
	corruptConfig, err := json.Marshal(invalidSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotConfig, corruptConfig, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = w.RestoreWorkload(ctx, &ateompb.RestoreWorkloadRequest{Atespace: "test", ActorName: "a", ActorUid: a.ActorUid, Spec: a.Spec, EgressGateway: gateway, Scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL, CpuMilli: 1000, MemoryBytes: 512 << 20, RuntimeAssetPaths: paths})
	if err == nil || !strings.Contains(err.Error(), "while reading snapshot net devices") {
		t.Fatalf("expected corrupt snapshot failure, got %v", err)
	}
	proxytest.CheckEgressInactive(t, proxy)
	if err := os.WriteFile(snapshotConfig, originalConfig, 0600); err != nil {
		t.Fatal(err)
	}

	_, err = w.RestoreWorkload(ctx, &ateompb.RestoreWorkloadRequest{Atespace: "test", ActorName: "a", ActorUid: a.ActorUid, Spec: a.Spec, EgressGateway: gateway, Scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL, CpuMilli: 1000, MemoryBytes: 512 << 20, RuntimeAssetPaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	checkVMMNetNS(t, w, a.ActorUid)
	proxytest.CheckCount(t, shared, "a", 2)
	if got := proxytest.Request(t, shared, "a", "/egress", 200); got != a.ActorUid+"/runtime" {
		t.Fatalf("restored egress: %s", got)
	}
	if _, err := w.TerminateWorkload(ctx, &ateompb.TerminateWorkloadRequest{Atespace: "test", ActorName: "a", ActorUid: a.ActorUid, Spec: a.Spec}); err != nil {
		t.Fatal(err)
	}
	b := proto.Clone(a).(*ateompb.RunWorkloadRequest)
	b.ActorName, b.ActorUid = "b", "b-runtime"
	proxytest.Bundle(t, workerUID, b.ActorUid, "app", os.Getenv("ATE_TEST_NETWORK_PROBE"))
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

func checkVMMNetNS(t *testing.T, w *AteomService, actorUID string) {
	t.Helper()
	ns, err := netns.GetFromPid(w.running[actorUID].chCmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	if !ns.Equal(w.proxy.Net.Gateway) {
		t.Fatal("VMM is not running in the gateway network namespace")
	}
}
