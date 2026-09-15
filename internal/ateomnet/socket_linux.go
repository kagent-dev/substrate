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
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/mdlayher/socket"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// ListenTCP creates an IPv4 TCP listener in ns. The caller retains ownership
// of ns and must keep it open until this call returns. Serving the listener
// does not require entering ns or retaining its handle.
func ListenTCP(ns netns.NsHandle, address netip.AddrPort) (*net.TCPListener, error) {
	c, sa, err := newTCPSocket(ns, address)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := c.SetsockoptInt(unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return nil, fmt.Errorf("setting TCP reuse address: %w", err)
	}
	if err := c.Bind(sa); err != nil {
		return nil, fmt.Errorf("binding TCP listener %s: %w", address, err)
	}
	if err := c.Listen(unix.SOMAXCONN); err != nil {
		return nil, fmt.Errorf("listening on TCP address %s: %w", address, err)
	}
	f, err := duplicateSocket(c)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lis, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("wrapping TCP listener: %w", err)
	}
	return lis.(*net.TCPListener), nil
}

// DialTCP creates its socket in ns, then connects after restoring the creating
// thread's namespace. The address must be IPv4; hostname resolution belongs to
// the caller. The caller owns ns and must keep it open until this call returns.
func DialTCP(ctx context.Context, ns netns.NsHandle, address netip.AddrPort) (*net.TCPConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if address.Port() == 0 {
		return nil, fmt.Errorf("TCP destination port must be nonzero")
	}
	c, sa, err := newTCPSocket(ns, address)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	// socket v0.5.0 ignores early cancellation when ctx has a deadline.
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = c.Close()
		close(canceled)
	})
	_, err = c.Connect(ctx, sa)
	if !stop() {
		<-canceled
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to TCP address %s: %w", address, err)
	}
	f, err := duplicateSocket(c)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	conn, err := net.FileConn(f)
	if err != nil {
		return nil, fmt.Errorf("wrapping TCP connection: %w", err)
	}
	return conn.(*net.TCPConn), nil
}

func newTCPSocket(ns netns.NsHandle, address netip.AddrPort) (*socket.Conn, *unix.SockaddrInet4, error) {
	// socket.Config treats zero as the current namespace, which must not be an
	// implicit fallback for a missing sandbox namespace.
	if ns <= 0 {
		return nil, nil, fmt.Errorf("TCP namespace handle must be positive")
	}
	if !address.IsValid() || !address.Addr().Is4() {
		return nil, nil, fmt.Errorf("TCP address must be IPv4: %s", address)
	}
	c, err := socket.Socket(unix.AF_INET, unix.SOCK_STREAM, unix.IPPROTO_TCP, "ateom-tcp", &socket.Config{NetNS: int(ns)})
	if err != nil {
		return nil, nil, fmt.Errorf("creating TCP socket in namespace: %w", err)
	}
	// socket v0.5.0 loses socket creation errors after restoring the namespace.
	if c == nil {
		return nil, nil, fmt.Errorf("creating TCP socket in namespace: socket creation failed without an error")
	}
	return c, &unix.SockaddrInet4{Addr: address.Addr().As4(), Port: int(address.Port())}, nil
}

// duplicateSocket bridges socket.Conn to the standard net FD wrappers without
// transferring ownership of socket.Conn's descriptor to an os.File finalizer.
func duplicateSocket(c *socket.Conn) (*os.File, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("accessing TCP socket: %w", err)
	}
	var fd int
	var dupErr error
	if err := raw.Control(func(original uintptr) {
		fd, dupErr = unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 0)
	}); err != nil {
		return nil, fmt.Errorf("accessing TCP descriptor: %w", err)
	}
	if dupErr != nil {
		return nil, fmt.Errorf("duplicating TCP descriptor: %w", dupErr)
	}
	return os.NewFile(uintptr(fd), "ateom-tcp"), nil
}
