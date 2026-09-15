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

// Package proxytest provides isolated Linux worker fixtures for runtime tests.
package proxytest

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/atenet"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/ateomproxy"
	"github.com/agent-substrate/substrate/internal/atunnel"
	"github.com/agent-substrate/substrate/internal/imagecache"
	"github.com/agent-substrate/substrate/internal/ocispec"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Enter reexecutes only this test in an isolated worker network/mount namespace.
// Runtime assets are opt-in, so ordinary root tests never download binaries.
func Enter(t *testing.T, assets ...string) bool {
	t.Helper()
	for _, asset := range assets {
		if os.Getenv(asset) == "" {
			t.Skipf("set %s to run the production runtime test", asset)
		}
	}
	if os.Getenv("ATEOM_RUNTIME_TEST_CHILD") != t.Name() {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-test.run=^"+t.Name()+"$", "-test.v", "-test.timeout=170s")
		cmd.Env = append(os.Environ(), "ATEOM_RUNTIME_TEST_CHILD="+t.Name())
		cmd.SysProcAttr = &unix.SysProcAttr{Cloneflags: unix.CLONE_NEWNET | unix.CLONE_NEWNS}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated runtime: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return false
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	eth := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", MTU: 1400}}
	if err := netlink.LinkAdd(eth); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(eth); err != nil {
		t.Fatal(err)
	}
	addr, err := netlink.ParseAddr("198.51.100.10/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(eth, addr); err != nil {
		t.Fatal(err)
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatal(err)
	}
	ateompath.ActorsDir = t.TempDir()
	t.Cleanup(func() {
		if err := imagecache.UnmountAllUnder(ateompath.ActorsDir); err != nil {
			t.Error(err)
		}
	})
	return true
}

func Config(workerUID string, microVM bool, bundle, trust string) ateomproxy.Config {
	upstream, _ := url.Parse("http://169.254.17.2:18081")
	return ateomproxy.Config{
		WorkerUID: workerUID, MicroVM: microVM,
		IngressAddress: "127.0.0.1:0", ConnectAddress: "127.0.0.1:0", CaptureAddress: "0.0.0.0:15001",
		Ingress:               atunnel.Config{CredentialBundlePath: bundle, TrustBundlePath: trust, AllowedClientID: "spiffe://runtime-test", Upstream: upstream},
		EgressTrustBundlePath: trust,
	}
}

// Bundle uses the real atelet OCI builder and runtime shaper, with a static probe
// in an extracted rootfs instead of fetching an image from a registry.
func Bundle(t *testing.T, workerUID, actorUID, name, probe string) {
	t.Helper()
	bundle := ateompath.OCIBundlePath(actorUID, name)
	root := filepath.Join(bundle, "rootfs")
	for _, dir := range []string{root, filepath.Join(root, "etc"), ateompath.PIDFileDir(actorUID), ateompath.RunSCStateDir(actorUID)} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.Open(probe)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenFile(filepath.Join(root, "probe"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	spec := ocispec.Build(ocispec.Options{ActorUID: actorUID, ContainerName: name, Args: []string{"/probe"}, NetNSPath: ateompath.AteomNetNSPath(workerUID)})
	if name == ocispec.PauseContainer {
		spec.Process.Args = []string{"/probe", "pause"}
	}
	if err := ocispec.Save(bundle, spec); err != nil {
		t.Fatal(err)
	}
}

func Spec() *ateompb.WorkloadSpec {
	return &ateompb.WorkloadSpec{Containers: []*ateompb.Container{{Name: "app", Readyz: &ateompb.Readyz{HttpGet: &ateompb.HTTPGetAction{Port: 18081, Path: "/dns"}, TimeoutSeconds: 30}}}}
}

func Request(t *testing.T, ingress *atunnel.Server, name, path string, wantCode int) string {
	t.Helper()
	r := httptest.NewRequest("GET", "http://worker"+path, nil)
	r.Header.Set(atenet.TargetActorHeader, "test/"+name)
	w := httptest.NewRecorder()
	ingress.ServeHTTP(w, r)
	if w.Code != wantCode {
		t.Fatalf("%s %s: status %d: %s", name, path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func CheckCount(t *testing.T, ingress *atunnel.Server, name string, want int) {
	t.Helper()
	if got := Request(t, ingress, name, "/count", 200); got != fmt.Sprint(want) {
		t.Fatalf("%s count=%s, want %d", name, got, want)
	}
}

// CheckEgressInactive verifies that DNS rejects a new tunneled connection.
func CheckEgressInactive(t *testing.T, proxy *ateomproxy.Proxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, err := proxy.Net.DialContext(ctx, "tcp", "127.0.0.1:53")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// Inactive egress closes immediately; active egress waits for a DNS query.
	var b [1]byte
	if n, err := conn.Read(b[:]); n != 0 || err != io.EOF {
		t.Fatalf("DNS accepted traffic after workload failure: read %d bytes, %v", n, err)
	}
}
