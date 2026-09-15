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

package proxytest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/internal/substratex509"
	"golang.org/x/net/dns/dnsmessage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type broker struct {
	ateletpb.UnimplementedCredentialBrokerServer
	ca              *localca.CA
	initialLifetime time.Duration
	mu              sync.Mutex
	minted          map[string]bool
}

func (b *broker) MintActorCertificate(ctx context.Context, r *ateletpb.MintActorCertificateRequest) (*ateletpb.MintActorCertificateResponse, error) {
	csr, err := x509.ParseCertificateRequest(r.CertificateSigningRequest)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	cert := certificateTemplate()
	b.mu.Lock()
	if b.initialLifetime > 0 && !b.minted[r.ActorUid] {
		cert.NotAfter = time.Now().Add(b.initialLifetime)
	}
	b.minted[r.ActorUid] = true
	b.mu.Unlock()
	if err := substratex509.AddActorIdentityToCertificate(&substratex509.ActorIdentity{Atespace: r.ActorAtespace, ActorName: r.ActorName, ActorUid: r.ActorUid, Purpose: substratex509.ActorIdentityPurposeAtunnel}, cert); err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, b.ca.RootCertificate, csr.PublicKey, b.ca.SigningKey)
	return &ateletpb.MintActorCertificateResponse{ActorCertificates: [][]byte{der}}, err
}
func certificateTemplate() *x509.Certificate {
	return &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
}

// Egress runs a test certificate broker and TLS CONNECT gateway. Runtime code
// still performs its real credential mint, namespace dial, capture, and relay.
func Egress(t *testing.T, initialActorLifetime ...time.Duration) (bundle, trust string, gateway *ateompb.EgressGateway) {
	t.Helper()
	ca, err := localca.GenerateCA("runtime-egress", localca.KeyTypeED25519, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(cert *x509.Certificate) tls.Certificate {
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca.RootCertificate, pub, ca.SigningKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}
	podCert := func(sa string) tls.Certificate {
		cert := certificateTemplate()
		uri, _ := url.Parse("spiffe://cluster.local/ns/ate-system/sa/" + sa)
		cert.URIs = []*url.URL{uri}
		if err := substratex509.AddPodIdentityToCertificate(&substratex509.PodIdentity{Namespace: "ate-system", ServiceAccountName: sa, ServiceAccountUID: sa + "-sa", PodName: sa, PodUID: sa + "-pod", NodeName: "test-node", NodeUID: "test-node-uid"}, cert); err != nil {
			t.Fatal(err)
		}
		return issue(cert)
	}
	worker, atelet := podCert("ateom"), podCert("atelet")
	bundle, trust = filepath.Join(t.TempDir(), "worker.pem"), filepath.Join(t.TempDir(), "trust.pem")
	key, err := x509.MarshalPKCS8PrivateKey(worker.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: worker.Certificate[0]}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
	if err := os.WriteFile(bundle, wire, 0600); err != nil {
		t.Fatal(err)
	}
	trustPEM, err := ca.TLSCertificateChainPEM()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trust, trustPEM, 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.RootCertificate)
	dir, err := os.MkdirTemp("", "broker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ateompath.CredentialBrokerSocket = filepath.Join(dir, "broker.sock")
	listener, err := net.Listen("unix", ateompath.CredentialBrokerSocket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{atelet}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots})))
	b := &broker{ca: ca, minted: make(map[string]bool)}
	if len(initialActorLifetime) > 0 {
		b.initialLifetime = initialActorLifetime[0]
	}
	ateletpb.RegisterCredentialBrokerServer(server, b)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	cert := certificateTemplate()
	cert.IPAddresses = []net.IP{net.ParseIP("198.51.100.10")}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{issue(cert)}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	tcp, err := net.Listen("tcp4", "198.51.100.10:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		identity, err := substratex509.ActorIdentityFromCertificate(r.TLS.PeerCertificates[0])
		if err != nil || identity == nil {
			http.Error(w, "identity required", http.StatusForbidden)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := rw.Flush(); err != nil {
			return
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if strings.HasSuffix(r.Host, ":53") {
			var length [2]byte
			if _, err := io.ReadFull(rw, length[:]); err != nil {
				return
			}
			query := make([]byte, binary.BigEndian.Uint16(length[:]))
			if _, err := io.ReadFull(rw, query); err != nil {
				return
			}
			var msg dnsmessage.Message
			if err := msg.Unpack(query); err != nil {
				return
			}
			msg.Response = true
			for _, q := range msg.Questions {
				if q.Type == dnsmessage.TypeA {
					msg.Answers = append(msg.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: q.Class, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{203, 0, 113, 9}}})
				}
			}
			response, err := msg.Pack()
			if err != nil {
				return
			}
			binary.BigEndian.PutUint16(length[:], uint16(len(response)))
			_, _ = conn.Write(append(length[:], response...))
			return
		}
		if r.Host != "198.51.100.10:18080" {
			t.Errorf("wrong original destination: %q", r.Host)
			return
		}
		_, _ = fmt.Fprintf(conn, "%s/", identity.ActorUid)
		_, _ = io.Copy(conn, rw)
	})}
	go func() { _ = httpServer.Serve(tls.NewListener(tcp, tlsConfig)) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	return bundle, trust, &ateompb.EgressGateway{Address: tcp.Addr().String()}
}
