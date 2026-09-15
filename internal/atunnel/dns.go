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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"golang.org/x/net/dns/dnsmessage"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// DNSResolver returns the worker's trusted IPv4 resolver. Sandbox DNS is always
// proxied to this endpoint, irrespective of the intercepted destination.
func DNSResolver() (string, error) {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" && net.ParseIP(f[1]).To4() != nil {
			return net.JoinHostPort(f[1], "53"), nil
		}
	}
	return "", fmt.Errorf("worker has no IPv4 DNS resolver")
}

// ServeDNS relays UDP queries over TCP to avoid truncation and also accepts
// native TCP DNS. Both listeners and all active requests close on cancellation.
// The dialer supplies actor-authenticated CONNECT streams, which carry TCP only.
// UDP queries therefore need DNS-over-TCP length framing on the upstream leg;
// native TCP clients can be relayed unchanged.
// ponytail: 64 concurrent DNS exchanges per sandbox; tune if resolver load warrants it.
func ServeDNS(ctx context.Context, udp *net.UDPConn, tcp net.Listener, resolver string, dial DialFunc) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = udp.Close(); _ = tcp.Close() })
	defer stop()
	slots := make(chan struct{}, 64)
	var wg sync.WaitGroup
	defer wg.Wait()
	handle := func(c net.Conn) {
		defer wg.Done()
		defer func() { <-slots }()
		defer c.Close()
		reqCtx, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		stop := context.AfterFunc(reqCtx, func() { _ = c.Close() })
		defer stop()
		upstream, err := dial(reqCtx, "tcp", resolver)
		if err != nil {
			return
		}
		defer upstream.Close()
		stopUp := context.AfterFunc(reqCtx, func() { _ = upstream.Close() })
		defer stopUp()
		copyBothWays(c, upstream)
	}
	acceptDone := make(chan error, 1)
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					err = nil
				}
				acceptDone <- err
				cancel()
				return
			}
			select {
			case slots <- struct{}{}:
				wg.Add(1)
				go handle(c)
			default:
				_ = c.Close()
			}
		}
	}()
	buf := make([]byte, 65535)
	for {
		size, peer, err := udp.ReadFromUDP(buf)
		if err != nil {
			stopping := ctx.Err() != nil
			cancel()
			if acceptErr := <-acceptDone; acceptErr != nil {
				return acceptErr
			}
			if errors.Is(err, net.ErrClosed) || stopping {
				return nil
			}
			return err
		}
		if size < 12 {
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			continue
		}
		packet := append([]byte(nil), buf[:size]...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			reqCtx, done := context.WithTimeout(ctx, 5*time.Second)
			defer done()
			c, err := dial(reqCtx, "tcp", resolver)
			if err != nil {
				return
			}
			defer c.Close()
			stop := context.AfterFunc(reqCtx, func() { _ = c.Close() })
			defer stop()
			// TCP DNS prefixes each message with its length; UDP has no prefix.
			wire := make([]byte, len(packet)+2)
			binary.BigEndian.PutUint16(wire, uint16(len(packet)))
			copy(wire[2:], packet)
			if _, err = c.Write(wire); err != nil {
				return
			}
			var length [2]byte
			if _, err = io.ReadFull(c, length[:]); err != nil {
				return
			}
			response := make([]byte, binary.BigEndian.Uint16(length[:]))
			if len(response) < 12 {
				return
			}
			if _, err = io.ReadFull(c, response); err != nil {
				return
			}
			if response[0] != packet[0] || response[1] != packet[1] {
				return
			}
			if len(response) > 512 {
				// Return a complete question with TC set so the client retries over
				// TCP. Slicing the response would leave malformed resource records.
				// Use the baseline UDP limit even if the query advertises EDNS.
				var parser dnsmessage.Parser
				header, err := parser.Start(packet)
				if err != nil {
					return
				}
				questions, err := parser.AllQuestions()
				if err != nil {
					return
				}
				header.Response, header.Truncated = true, true
				builder := dnsmessage.NewBuilder(nil, header)
				if err := builder.StartQuestions(); err != nil {
					return
				}
				for _, q := range questions {
					if err := builder.Question(q); err != nil {
						return
					}
				}
				response, err = builder.Finish()
				if err != nil || len(response) > 512 {
					return
				}
			}
			_, _ = udp.WriteToUDP(response, peer)
		}()
	}
}
