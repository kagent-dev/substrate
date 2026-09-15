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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/ateomnet"
	"github.com/agent-substrate/substrate/internal/roottest"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Runtime binaries and a statically built testdata/network-probe are supplied
// explicitly so ordinary root tests do not download or install runtimes.
func TestSandboxGatewayGVisor(t *testing.T) {
	roottest.Require(t, "real gVisor sandboxes")
	runsc, probe := runtimeTestAsset(t, "ATE_TEST_RUNSC"), runtimeTestAsset(t, "ATE_TEST_NETWORK_PROBE")
	worker, destination := newSocketTestWorker(t)
	newClient := serveNamespaceTLSGateway(t, *worker, destination)
	var gateways [2]*netns.NsHandle
	var stop [2]func()
	var restore func()
	for i := range gateways {
		gateway, actor, _ := setupSocketTestGateway(t, *worker, i, "veth")
		setupSocketTestTunnel(t, *gateway, i)
		gateways[i] = gateway
		serveCapturedEgressDialer(t, *gateway, destination, newClient(*gateway, i))
		dir := runtimeTestDir(t)
		root := filepath.Join(dir, "rootfs")
		copyRuntimeProbe(t, probe, root)
		spec := specs.Spec{
			Version: specs.Version,
			Root:    &specs.Root{Path: root},
			Process: &specs.Process{Args: []string{"/probe"}, Cwd: "/", Env: []string{"PATH=/"}},
			Mounts:  []specs.Mount{{Destination: "/proc", Type: "proc", Source: "proc"}},
			Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace}, {Type: specs.MountNamespace},
				{Type: specs.UTSNamespace}, {Type: specs.IPCNamespace},
				{Type: specs.NetworkNamespace, Path: fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), *actor)},
			}},
		}
		data, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		args := []string{"--root=" + filepath.Join(dir, "state"), "--ignore-cgroups", "--network=sandbox", "--platform=systrap"}
		remove := func() { runtimeTestCommand(t, runsc, append(args, "delete", "--force", "probe")...) }
		stop[i] = sync.OnceFunc(remove)
		t.Cleanup(stop[i])
		runtimeTestCommand(t, runsc, append(args, "create", "--bundle="+dir, "probe")...)
		runtimeTestCommand(t, runsc, append(args, "start", "probe")...)
		waitRuntimeProbe(t, *gateway)
		// runsc has consumed the runtime namespace's addresses. Only the
		// gateway namespace still hosts a Linux IP stack for our proxy.
		inSocketTestNamespace(t, *actor, func() error {
			link, err := netlink.LinkByName("eth0")
			if err != nil {
				return err
			}
			addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
			if err == nil && len(addresses) != 0 {
				return fmt.Errorf("runsc left host IPv4 addresses: %v", addresses)
			}
			return err
		})
		if i == 0 {
			restore = func() {
				if got, err := runtimeProbeRequest(*gateway, "/count"); err != nil || got != "1" {
					t.Fatalf("before checkpoint: %q, %v", got, err)
				}
				snapshot := filepath.Join(dir, "snapshot")
				runtimeTestCommand(t, runsc, append(args, "checkpoint", "--image-path="+snapshot, "probe")...)
				stop[i]()
				// Re-enroll the network that runsc consumed on create. Restore
				// must rebuild its netstack from the same sandbox-facing config.
				inSocketTestNamespace(t, *actor, func() error {
					link, err := netlink.LinkByName("eth0")
					if err != nil {
						return err
					}
					if err := configureSocketTestLink(link, ateomnet.ActorVethCIDR); err != nil {
						return err
					}
					return netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(ateomnet.ActorVethGateway)})
				})
				stop[i] = sync.OnceFunc(remove)
				t.Cleanup(stop[i])
				runtimeTestCommand(t, runsc, append(args, "restore", "--bundle="+dir, "--image-path="+snapshot, "--detach", "probe")...)
				waitRuntimeProbe(t, *gateway)
				if got, err := runtimeProbeRequest(*gateway, "/count"); err != nil || got != "2" {
					t.Fatalf("after checkpoint: %q, %v", got, err)
				}
			}
		}
	}
	checkRuntimeEgress(t, gateways)
	restoreWithPeerTraffic(t, *gateways[1], restore)
	checkRuntimeEgress(t, gateways)
	stop[0]()
	if got, err := runtimeProbeRequest(*gateways[1], "/egress"); err != nil || got != "remote/192.0.2.6/runtime" {
		t.Fatalf("gVisor B after removing A: %q, %v", got, err)
	}
}

func TestSandboxGatewayMicroVM(t *testing.T) {
	roottest.Require(t, "real Cloud Hypervisor VMs and direct TAP sockets")
	vmm := runtimeTestAsset(t, "ATE_TEST_CLOUD_HYPERVISOR")
	kernel, probe := runtimeTestAsset(t, "ATE_TEST_VM_KERNEL"), runtimeTestAsset(t, "ATE_TEST_NETWORK_PROBE")
	worker, destination := newSocketTestWorker(t)
	newClient := serveNamespaceTLSGateway(t, *worker, destination)
	var gateways [2]*netns.NsHandle
	var stop [2]func()
	var restore func()
	for i := range gateways {
		gateway := setupSocketTestTransit(t, *worker, i)
		gateways[i] = gateway
		tap := runtimeTestTAP(t, *gateway)
		setupSocketTestTunnel(t, *gateway, i)
		serveCapturedEgressDialer(t, *gateway, destination, newClient(*gateway, i))
		dir := runtimeTestDir(t)
		root, disk := filepath.Join(dir, "rootfs"), filepath.Join(dir, "rootfs.img")
		copyRuntimeProbe(t, probe, root)
		file, err := os.Create(disk)
		if err != nil {
			t.Fatal(err)
		}
		err = file.Truncate(64 << 20)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		runtimeTestCommand(t, "/usr/sbin/mkfs.ext4", "-q", "-F", "-d", root, disk)
		api := filepath.Join(dir, "api.sock")
		stop[i] = startRuntimeVMM(t, vmm, *gateway, tap, dir,
			"--api-socket", api, "--kernel", kernel, "--cpus", "boot=1", "--memory", "size=128M,shared=on",
			"--disk", "path="+disk+",image_type=raw", "--console", "off", "--serial", "file="+filepath.Join(dir, "serial.log"),
			"--cmdline", "console=ttyS0 root=/dev/vda rw init=/probe -- guest",
			"--net", "fd=[3],mac=02:a8:1e:00:00:02,num_queues=2,id=net0")
		waitRuntimeProbe(t, *gateway)
		if got, err := runtimeProbeRequest(*gateway, "/mtu"); err != nil || got != "1400" {
			t.Fatalf("guest MTU: %q, %v", got, err)
		}
		if i == 0 {
			restore = func() {
				// Saving and restoring a live listener must retain the in-memory
				// counter and continue using the supplied namespace-associated TAP.
				if got, err := runtimeProbeRequest(*gateway, "/count"); err != nil || got != "1" {
					t.Fatalf("before snapshot: %q, %v", got, err)
				}
				snapshot := filepath.Join(dir, "snapshot")
				if err := os.Mkdir(snapshot, 0o700); err != nil {
					t.Fatal(err)
				}
				runtimeVMRequest(t, api, "vm.pause", nil)
				runtimeVMRequest(t, api, "vm.snapshot", map[string]any{"destination_url": "file://" + snapshot})
				stop[i]()
				if err := os.Remove(api); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if err := tap.Close(); err != nil {
					t.Fatal(err)
				}
				tap = runtimeTestTAP(t, *gateway)
				stop[i] = startRuntimeVMM(t, vmm, *gateway, tap, dir, "--api-socket", api, "--restore", "source_url=file://"+snapshot+",net_fds=[net0@[3]],resume=true")
				waitRuntimeProbe(t, *gateway)
				if got, err := runtimeProbeRequest(*gateway, "/count"); err != nil || got != "2" {
					t.Fatalf("after snapshot: %q, %v", got, err)
				}
				if got, err := runtimeProbeRequest(*gateway, "/mtu"); err != nil || got != "1400" {
					t.Fatalf("restored guest MTU: %q, %v", got, err)
				}
			}
		}
	}
	checkRuntimeEgress(t, gateways)
	restoreWithPeerTraffic(t, *gateways[1], restore)
	checkRuntimeEgress(t, gateways)
	stop[0]()
	if got, err := runtimeProbeRequest(*gateways[1], "/egress"); err != nil || got != "remote/192.0.2.6/runtime" {
		t.Fatalf("VM B after stopping A: %q, %v", got, err)
	}
}

func runtimeTestAsset(t *testing.T, name string) string {
	t.Helper()
	path := os.Getenv(name)
	if path == "" {
		t.Skipf("set %s to enable runtime tests", name)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func runtimeTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "atnet-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// runsc keeps this bind mount in its shared state directory after
		// deleting the last sandbox. This directory belongs only to this test.
		if err := unix.Unmount(filepath.Join(dir, "state", "null-netns"), unix.MNT_DETACH); err != nil && err != unix.ENOENT && err != unix.EINVAL {
			t.Error(err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func copyRuntimeProbe(t *testing.T, probe, root string) {
	t.Helper()
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "probe"), data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func runtimeTestCommand(t *testing.T, binary string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Runtime children can retain stdout after the CLI exits. A pipe would
	// make Cmd.Wait wait for those children instead of just the CLI.
	log, err := os.CreateTemp("", "atnet-command-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(log.Name())
	defer log.Close()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		out, _ := os.ReadFile(log.Name())
		t.Fatalf("%s %v: %v\n%s", binary, args, err, out)
	}
}

func runtimeTestTAP(t *testing.T, ns netns.NsHandle) *os.File {
	t.Helper()
	tap := socketTestTAP(t, ns, "ateom0", ateomnet.HostVethCIDR, "02:a8:1e:00:00:01")
	inSocketTestNamespace(t, ns, func() error {
		link, err := netlink.LinkByName("ateom0")
		if err != nil {
			return err
		}
		return netlink.LinkSetMTU(link, 1400)
	})
	return tap
}

func startRuntimeVMM(t *testing.T, binary string, ns netns.NsHandle, tap *os.File, dir string, args ...string) func() {
	t.Helper()
	log, err := os.OpenFile(filepath.Join(dir, "vmm.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// CH queries TAP properties by interface name through a control socket.
	// Its process must use the gateway namespace even with an inherited TAP FD.
	cmd := exec.Command("nsenter", append([]string{fmt.Sprintf("--net=/proc/%d/fd/%d", os.Getpid(), ns), binary}, args...)...)
	cmd.ExtraFiles = []*os.File{tap}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = log.Close() })
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			for _, name := range []string{"vmm.log", "serial.log"} {
				data, _ := os.ReadFile(filepath.Join(dir, name))
				t.Logf("%s:\n%s", name, data)
			}
		}
	})
	return stop
}

func runtimeVMRequest(t *testing.T, socket, method string, body any) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPut, "http://vmm/api/v1/"+method, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: %s: %s", method, resp.Status, data)
	}
}

func runtimeProbeRequest(ns netns.NsHandle, path string) (string, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return ateomnet.DialTCP(ctx, ns, netip.MustParseAddrPort("169.254.17.2:18081"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second}
	resp, err := client.Get("http://probe" + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("probe status: %s", resp.Status)
	}
	return string(data), err
}

func waitRuntimeProbe(t *testing.T, ns netns.NsHandle) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := runtimeProbeRequest(ns, "/egress")
		if err == nil && strings.HasSuffix(got, "/runtime") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime not ready: %q, %v", got, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func checkRuntimeEgress(t *testing.T, gateways [2]*netns.NsHandle) {
	t.Helper()
	var wg sync.WaitGroup
	for i := range gateways {
		for range 10 {
			wg.Go(func() {
				want := fmt.Sprintf("remote/192.0.2.%d/runtime", 4*i+2)
				if got, err := runtimeProbeRequest(*gateways[i], "/egress"); err != nil || got != want {
					t.Errorf("sandbox %d: %q, %v; want %q", i, got, err, want)
				}
			})
		}
	}
	wg.Wait()
}

func restoreWithPeerTraffic(t *testing.T, peer netns.NsHandle, restore func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			got, err := runtimeProbeRequest(peer, "/egress")
			if err != nil || got != "remote/192.0.2.6/runtime" {
				t.Errorf("peer during restore: %q, %v", got, err)
				return
			}
		}
	})
	defer func() { cancel(); wg.Wait() }()
	restore()
}
