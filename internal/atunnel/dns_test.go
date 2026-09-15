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

// Package atunnel carries actor ingress and egress through an ateom worker pod.
package atunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSListenerFailure(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	if err := tcp.SetDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ServeDNS(ctx, udp, tcp, "unused:53", nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("ServeDNS error = %v; want listener deadline error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("TCP listener failure did not stop DNS")
	}
	if _, _, err := udp.ReadFromUDP(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("UDP listener remains open after TCP failure: %v", err)
	}
}

func TestDNSTCPHalfCloseThroughEgress(t *testing.T) {
	resolver, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	resolverDone := make(chan struct{})
	go func() {
		defer close(resolverDone)
		conn, err := resolver.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		// Wait for the query's FIN before answering, so a full close loses the answer.
		query, err := io.ReadAll(conn)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := conn.Write(query); err != nil {
			t.Error(err)
		}
	}()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	egress, err := NewEgress(TCPOriginalDestination)
	if err != nil {
		t.Fatal(err)
	}
	if err := egress.Activate(egressDialerFunc(func(ctx context.Context, destination string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", destination)
	}), fakeActorCertificateSource{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- ServeDNS(ctx, udp, tcp, resolver.Addr().String(), egress.DialContext) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		if err := egress.Deactivate(context.Background()); err != nil {
			t.Error(err)
		}
		_ = resolver.Close()
		<-resolverDone
	})
	stream, err := net.Dial("tcp4", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
	query := append([]byte{0, 12}, make([]byte, 12)...)
	if _, err := stream.Write(query); err != nil {
		t.Fatal(err)
	}
	if err := stream.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	answer, err := io.ReadAll(stream)
	if err != nil || !bytes.Equal(answer, query) {
		t.Fatalf("answer after half-close = %x, %v; want %x", answer, err, query)
	}
}

func TestDNSUDPTruncationAndTCP(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "trusted:53" {
			t.Errorf("DNS escaped trusted resolver: %s %s", network, address)
		}
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			var length [2]byte
			if _, err := io.ReadFull(b, length[:]); err != nil {
				return
			}
			q := make([]byte, binary.BigEndian.Uint16(length[:]))
			if _, err := io.ReadFull(b, q); err != nil {
				return
			}
			// Oversize upstream answers must trigger a legal TCP fallback for UDP.
			reply := append(q, make([]byte, 700)...)
			reply[2] |= 0x80
			binary.BigEndian.PutUint16(length[:], uint16(len(reply)))
			_, _ = b.Write(append(length[:], reply...))
		}()
		return a, nil
	}
	go func() { done <- ServeDNS(ctx, udp, tcp, "trusted:53", dial) }()
	name, err := dnsmessage.NewName("example.test.")
	if err != nil {
		t.Fatal(err)
	}
	query, err := (&dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("udp4", udp.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write(query); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4096)
	n, err := c.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	var response dnsmessage.Message
	if err := response.Unpack(b[:n]); err != nil {
		t.Fatal(err)
	}
	if !response.Truncated || !response.Response || response.ID != 7 || len(response.Questions) != 1 || n > 512 {
		t.Fatalf("invalid UDP fallback: %+v", response)
	}
	stream, err := net.Dial("tcp4", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(query)))
	if _, err := stream.Write(append(length[:], query...)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(stream, length[:]); err != nil {
		t.Fatal(err)
	}
	if got := int(binary.BigEndian.Uint16(length[:])); got != len(query)+700 {
		t.Fatalf("TCP answer shortened: %d", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("DNS cancellation leaked handlers")
	}
}
