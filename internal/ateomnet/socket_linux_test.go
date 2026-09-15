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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/ateomnet"
	"github.com/agent-substrate/substrate/internal/roottest"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const socketTestTimeout = 10 * time.Second

func TestNamespaceTCPValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ns      netns.NsHandle
		address netip.AddrPort
	}{
		{"missing namespace", 0, netip.MustParseAddrPort("127.0.0.1:1234")},
		{"closed namespace", netns.None(), netip.MustParseAddrPort("127.0.0.1:1234")},
		{"missing address", 1, netip.AddrPort{}},
		{"IPv6", 1, netip.MustParseAddrPort("[::1]:1234")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if lis, err := ateomnet.ListenTCP(tc.ns, tc.address); err == nil {
				lis.Close()
				t.Fatal("ListenTCP accepted invalid input")
			}
			if conn, err := ateomnet.DialTCP(t.Context(), tc.ns, tc.address); err == nil {
				conn.Close()
				t.Fatal("DialTCP accepted invalid input")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ateomnet.DialTCP(ctx, 1, netip.MustParseAddrPort("127.0.0.1:1234")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled dial: %v", err)
	}
	if _, err := ateomnet.DialTCP(t.Context(), 1, netip.MustParseAddrPort("127.0.0.1:0")); err == nil {
		t.Fatal("DialTCP accepted destination port zero")
	}
}

func TestNamespaceTCPSameAddress(t *testing.T) {
	roottest.Require(t, "network namespaces and namespace-associated TCP sockets")
	a, b := newSocketTestNamespace(t), newSocketTestNamespace(t)
	address := netip.MustParseAddrPort("127.0.0.1:15001")
	stopA := serveNamespaceEcho(t, *a, address, "a")
	serveNamespaceEcho(t, *b, address, "b")
	for _, tc := range []struct {
		ns   netns.NsHandle
		want string
	}{{*a, "a/127.0.0.1/hello"}, {*b, "b/127.0.0.1/hello"}} {
		if got := exchangeNamespaceTCP(t, dialNamespaceTCP(t, tc.ns, address), "hello"); got != tc.want {
			t.Fatalf("echo = %q, want %q", got, tc.want)
		}
	}
	// The socket retains its namespace even after the caller releases the FD.
	connB := dialNamespaceTCP(t, *b, address)
	defer connB.Close()
	stopA()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if got := exchangeNamespaceTCP(t, connB, "still here"); got != "b/127.0.0.1/still here" {
		t.Fatalf("surviving connection = %q", got)
	}
}

func TestNamespaceTCPErrorsReleaseDescriptors(t *testing.T) {
	roottest.Require(t, "network namespaces and TCP socket error handling")
	ns := newSocketTestNamespace(t)
	address := netip.MustParseAddrPort("127.0.0.1:15001")
	serveNamespaceEcho(t, *ns, address, "warmup")
	exchangeNamespaceTCP(t, dialNamespaceTCP(t, *ns, address), "warmup")
	badNS, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer badNS.Close()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	before := socketDescriptorCount(t)
	for range 20 {
		lis, err := ateomnet.ListenTCP(*ns, netip.MustParseAddrPort("127.0.0.1:0"))
		if err != nil {
			t.Fatal(err)
		}
		conn := dialNamespaceTCP(t, *ns, lis.Addr().(*net.TCPAddr).AddrPort())
		peer, err := lis.AcceptTCP()
		conn.Close()
		lis.Close()
		if err != nil {
			t.Fatal(err)
		}
		peer.Close()
		if _, err := ateomnet.ListenTCP(netns.NsHandle(badNS.Fd()), address); err == nil {
			t.Fatal("accepted a non-namespace FD")
		}
		if _, err := ateomnet.ListenTCP(*ns, address); !errors.Is(err, unix.EADDRINUSE) {
			t.Fatalf("duplicate listener: %v", err)
		}
		if _, err := ateomnet.ListenTCP(*ns, netip.MustParseAddrPort("192.0.2.1:15001")); !errors.Is(err, unix.EADDRNOTAVAIL) {
			t.Fatalf("unassigned listener address: %v", err)
		}
		if _, err := ateomnet.DialTCP(t.Context(), *ns, netip.MustParseAddrPort("127.0.0.1:15002")); !errors.Is(err, unix.ECONNREFUSED) {
			t.Fatalf("refused connection: %v", err)
		}
	}
	if after := socketDescriptorCount(t); after != before {
		t.Fatalf("socket operations leaked descriptors: before=%d after=%d", before, after)
	}
	current, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if !current.Equal(original) {
		t.Fatal("socket operations changed the caller's namespace")
	}
	exchangeNamespaceTCP(t, dialNamespaceTCP(t, *ns, address), "after errors")
}

func TestNamespaceTCPConnectDeadline(t *testing.T) {
	roottest.Require(t, "network namespaces and TCP connect cancellation")
	ns := newSocketTestNamespace(t)
	inSocketTestNamespace(t, *ns, func() error {
		link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "blackhole"}}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		if err := netlink.AddrAdd(link, ateomnet.MustParseAddr("192.0.2.1/24")); err != nil {
			return err
		}
		return netlink.LinkSetUp(link)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if conn, err := ateomnet.DialTCP(ctx, *ns, netip.MustParseAddrPort("192.0.2.2:80")); err == nil {
		conn.Close()
		t.Fatal("blackhole connection succeeded")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connect deadline: %v", err)
	}
	t.Run("cancel before deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			conn, err := ateomnet.DialTCP(ctx, *ns, netip.MustParseAddrPort("192.0.2.2:80"))
			if conn != nil {
				conn.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("blackhole dial finished before cancellation: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled dial: %v", err)
			}
		case <-time.After(time.Second):
			<-done
			t.Fatal("dial ignored cancellation until its deadline")
		}
	})
}

func TestNamespaceTCPSocketExhaustion(t *testing.T) {
	roottest.Require(t, "socket creation failure inside a network namespace")
	// Descriptor limits are process-wide; isolate exhaustion from other tests.
	if os.Getenv("ATE_TEST_SOCKET_EXHAUSTION") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestNamespaceTCPSocketExhaustion$", "-test.timeout=30s")
		cmd.Env = append(os.Environ(), "ATE_TEST_SOCKET_EXHAUSTION=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("socket exhaustion subprocess: %v\n%s", err, output)
		}
		return
	}
	ns := newSocketTestNamespace(t)
	address := netip.MustParseAddrPort("127.0.0.1:15001")
	// Initialize the poller before constraining descriptor allocation.
	lis, err := ateomnet.ListenTCP(*ns, address)
	if err != nil {
		t.Fatal(err)
	}
	lis.Close()
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	for _, dial := range []bool{false, true} {
		func() {
			fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			unix.Close(fd)
			// Leave one slot for the saved thread namespace, none for socket().
			if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: uint64(fd + 1), Max: limit.Max}); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
					t.Fatal(err)
				}
			}()
			if dial {
				_, err = ateomnet.DialTCP(t.Context(), *ns, address)
			} else {
				_, err = ateomnet.ListenTCP(*ns, address)
			}
			if err == nil {
				t.Fatal("socket exhaustion did not return an error")
			}
		}()
	}
	lis, err = ateomnet.ListenTCP(*ns, address)
	if err != nil {
		t.Fatalf("listener after recovering descriptors: %v", err)
	}
	lis.Close()
}

func newSocketTestNamespace(t *testing.T) *netns.NsHandle {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	defer func() {
		if err := netns.Set(original); err != nil {
			panic(fmt.Sprintf("restoring test namespace: %v", err))
		}
	}()
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ns.IsOpen() {
			if err := ns.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatal(err)
	}
	return &ns
}

func inSocketTestNamespace(t *testing.T, ns netns.NsHandle, fn func() error) {
	t.Helper()
	if err := ateomnet.NetNSDo(t.Context(), ns, func(context.Context) error { return fn() }); err != nil {
		t.Fatal(err)
	}
}

func serveNamespaceEcho(t *testing.T, ns netns.NsHandle, address netip.AddrPort, name string) func() {
	t.Helper()
	lis, err := ateomnet.ListenTCP(ns, address)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		var handlers sync.WaitGroup
		defer close(done)
		defer handlers.Wait()
		for {
			conn, err := lis.AcceptTCP()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("accept: %v", err)
				}
				return
			}
			handlers.Go(func() {
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				_ = conn.SetDeadline(time.Now().Add(socketTestTimeout))
				_, err := fmt.Fprintf(conn, "%s/%s/", name, conn.RemoteAddr().(*net.TCPAddr).IP)
				if err == nil {
					_, err = io.Copy(conn, io.LimitReader(conn, 1024))
				}
				if err != nil {
					if ctx.Err() == nil {
						t.Errorf("echo: %v", err)
					}
					return
				}
			})
		}
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		_ = lis.Close()
		<-done
	})
	t.Cleanup(stop)
	return stop
}

func dialNamespaceTCP(t *testing.T, ns netns.NsHandle, address netip.AddrPort) *net.TCPConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), socketTestTimeout)
	defer cancel()
	conn, err := ateomnet.DialTCP(ctx, ns, address)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(socketTestTimeout)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return conn
}

func exchangeNamespaceTCP(t *testing.T, conn *net.TCPConn, message string) string {
	t.Helper()
	defer conn.Close()
	if _, err := io.WriteString(conn, message); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func socketDescriptorCount(t *testing.T) int {
	t.Helper()
	files, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	// Count the descriptors owned by namespace socket operations. Go may
	// independently release cached splice pipes from earlier proxy tests.
	count := 0
	for _, file := range files {
		target, err := os.Readlink("/proc/self/fd/" + file.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(target, "socket:[") || strings.HasPrefix(target, "net:[") {
			count++
		}
	}
	return count
}
