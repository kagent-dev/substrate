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
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/ateomnet"
	"github.com/agent-substrate/substrate/internal/atunnel"
	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/agent-substrate/substrate/internal/roottest"
	"github.com/vishvananda/netns"
)

func TestSandboxGatewayTLS(t *testing.T) {
	roottest.Require(t, "intercepted TCP through namespace-local mTLS CONNECT clients")
	worker, destination := newSocketTestWorker(t)
	newClient := serveNamespaceTLSGateway(t, *worker, destination)
	for slot := range 2 {
		t.Run(fmt.Sprintf("sandbox%d", slot), func(t *testing.T) {
			t.Parallel()
			gateway, actor, _ := setupSocketTestGateway(t, *worker, slot, "veth")
			setupSocketTestTunnel(t, *gateway, slot)
			stop, captures := serveCapturedEgressDialer(t, *gateway, destination, newClient(*gateway, slot))
			var wg sync.WaitGroup
			for i := range 32 {
				wg.Go(func() {
					message := strings.Repeat(fmt.Sprintf("sandbox%d-message%d;", slot, i), 4096)
					got := exchangeNamespaceTCP(t, dialNamespaceTCP(t, *actor, destination), message)
					want := fmt.Sprintf("remote/192.0.2.%d/%s", slot*4+2, message)
					if got != want {
						t.Errorf("TLS payload mismatch: got %d bytes, want %d", len(got), len(want))
					}
				})
			}
			wg.Wait()
			if got := captures.Load(); got != 32 {
				t.Fatalf("captured %d connections, want 32", got)
			}
			stop()
			// Capture remains installed while the proxy is down; the actor
			// must fail instead of bypassing it through the transit interface.
			conn, err := ateomnet.DialTCP(t.Context(), *actor, destination)
			if err == nil {
				conn.Close()
				t.Fatal("egress bypassed a stopped proxy")
			}
			// Replacing the proxy listener must not require replacing the netns.
			serveCapturedEgressDialer(t, *gateway, destination, newClient(*gateway, slot))
			got := exchangeNamespaceTCP(t, dialNamespaceTCP(t, *actor, destination), "restart")
			if want := fmt.Sprintf("remote/192.0.2.%d/restart", slot*4+2); got != want {
				t.Fatalf("proxy restart: %q, want %q", got, want)
			}
		})
	}
}

// The TLS endpoint verifies client identity against the namespace's source IP,
// verifies the original CONNECT authority, then echoes the tunneled stream.
// It uses the production atunnel Client; certificate issuance is a test CA.
func serveNamespaceTLSGateway(t *testing.T, worker netns.NsHandle, destination netip.AddrPort) func(netns.NsHandle, int) *atunnel.Client {
	t.Helper()
	ca, err := localca.GenerateCA("namespace-test", localca.KeyTypeED25519, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := ca.TLSCertificateChainPEM()
	if err != nil {
		t.Fatal(err)
	}
	trustPath := filepath.Join(t.TempDir(), "trust.pem")
	if err := os.WriteFile(trustPath, trust, 0o600); err != nil {
		t.Fatal(err)
	}
	issue := func(slot int) tls.Certificate {
		key, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := url.Parse(fmt.Sprintf("spiffe://substrate-actor.local/atespace/test/actor/%d", slot))
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			DNSNames: []string{"egress.test"}, URIs: []*url.URL{identity}, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca.RootCertificate, key, ca.SigningKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
	}
	serverCert := issue(-1)
	pool := x509.NewCertPool()
	pool.AddCert(ca.RootCertificate)
	listener, err := ateomnet.ListenTCP(worker, netip.MustParseAddrPort("198.51.100.20:18443"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var accept, handlers sync.WaitGroup
	accept.Go(func() {
		for {
			raw, err := listener.AcceptTCP()
			if err != nil {
				if ctx.Err() == nil {
					t.Error(err)
				}
				return
			}
			handlers.Go(func() {
				defer raw.Close()
				stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
				defer stop()
				_ = raw.SetDeadline(time.Now().Add(socketTestTimeout))
				conn := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool})
				if err := conn.HandshakeContext(ctx); err != nil {
					t.Error(err)
					return
				}
				ip := raw.RemoteAddr().(*net.TCPAddr).IP.String()
				slot, ok := map[string]int{"192.0.2.2": 0, "192.0.2.6": 1}[ip]
				if !ok {
					t.Errorf("unexpected gateway source IP: %s", ip)
					return
				}
				wantURI := fmt.Sprintf("spiffe://substrate-actor.local/atespace/test/actor/%d", slot)
				uris := conn.ConnectionState().PeerCertificates[0].URIs
				if len(uris) != 1 || uris[0].String() != wantURI {
					t.Errorf("source %s has wrong client identity: %v", ip, uris)
					return
				}
				reader := bufio.NewReader(conn)
				req, err := http.ReadRequest(reader)
				if err != nil {
					t.Error(err)
					return
				}
				if req.Method != http.MethodConnect || req.Host != destination.String() {
					t.Errorf("unexpected CONNECT: %s %s", req.Method, req.Host)
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					t.Error(err)
					return
				}
				data, err := io.ReadAll(io.LimitReader(reader, 2<<20))
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := fmt.Fprintf(conn, "remote/%s/%s", ip, data); err != nil {
					t.Error(err)
				}
				_ = conn.CloseWrite()
			})
		}
	})
	t.Cleanup(func() { cancel(); _ = listener.Close(); accept.Wait(); handlers.Wait() })
	return func(ns netns.NsHandle, slot int) *atunnel.Client {
		cert := issue(slot)
		client, err := atunnel.NewClient(atunnel.ClientConfig{GatewayAddress: listener.Addr().String(), ServerName: "egress.test", TrustBundlePath: trustPath,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }},
			atunnel.WithDialer(func(ctx context.Context, _, address string) (net.Conn, error) {
				addr, err := netip.ParseAddrPort(address)
				if err != nil {
					return nil, err
				}
				return ateomnet.DialTCP(ctx, ns, addr)
			}))
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
}
