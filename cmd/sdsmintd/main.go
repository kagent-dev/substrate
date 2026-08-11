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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	secretv3 "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/agent-substrate/substrate/internal/localca"
)

const leafLifetime = 5 * time.Minute

type server struct {
	secretv3.UnimplementedSecretDiscoveryServiceServer
	ca  *localca.CA
	now func() time.Time
}

func (s *server) DeltaSecrets(stream secretv3.SecretDiscoveryService_DeltaSecretsServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, name := range req.GetResourceNamesSubscribe() {
			resource, err := s.mint(name)
			if err != nil {
				if err := stream.Send(&discoveryv3.DeltaDiscoveryResponse{
					TypeUrl: resourcev3.SecretType, RemovedResources: []string{name}, Nonce: nonce(),
				}); err != nil {
					return err
				}
				continue
			}
			if err := stream.Send(&discoveryv3.DeltaDiscoveryResponse{
				SystemVersionInfo: resource.GetVersion(), TypeUrl: resourcev3.SecretType,
				Resources: []*discoveryv3.Resource{resource}, Nonce: nonce(),
			}); err != nil {
				return err
			}
		}
	}
}

func (s *server) mint(name string) (*discoveryv3.Resource, error) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name == "" || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, fmt.Errorf("invalid SNI")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := s.now()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(leafLifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, s.ca.RootCertificate, &key.PublicKey, s.ca.SigningKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	secret := &tlsv3.Secret{Name: name, Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
		CertificateChain: &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: certPEM}},
		PrivateKey:       &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: keyPEM}},
	}}}
	packed, err := anypb.New(secret)
	if err != nil {
		return nil, err
	}
	return &discoveryv3.Resource{
		Name: name, Version: serial.Text(16), Resource: packed, Ttl: durationpb.New(leafLifetime),
	}, nil
}

func nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func main() {
	socket := flag.String("socket", "/run/sdsmintd/sds.sock", "Unix socket to serve SDS on")
	poolFile := flag.String("ca-pool", "/run/egress-ca/pool.json", "interception CA pool")
	flag.Parse()

	b, err := os.ReadFile(*poolFile)
	if err != nil {
		panic(err)
	}
	pool, err := localca.Unmarshal(b)
	if err != nil || len(pool.CAs) == 0 {
		panic("interception CA pool is empty or invalid")
	}
	if err := os.MkdirAll(filepath.Dir(*socket), 0o750); err != nil {
		panic(err)
	}
	if err := os.Remove(*socket); err != nil && !os.IsNotExist(err) {
		panic(err)
	}
	lis, err := net.Listen("unix", *socket)
	if err != nil {
		panic(err)
	}
	if err := os.Chmod(*socket, 0o660); err != nil {
		panic(err)
	}

	grpcServer := grpc.NewServer()
	secretv3.RegisterSecretDiscoveryServiceServer(grpcServer, &server{ca: pool.CAs[0], now: time.Now})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()
	if err := grpcServer.Serve(lis); err != nil {
		panic(err)
	}
}
