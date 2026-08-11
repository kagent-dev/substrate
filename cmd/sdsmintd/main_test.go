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
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"

	"github.com/agent-substrate/substrate/internal/localca"
)

func TestMint(t *testing.T) {
	ca, err := localca.GenerateED25519CA("egress-test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	resource, err := (&server{ca: ca, now: func() time.Time { return now }}).mint("API.Example.com.")
	if err != nil {
		t.Fatal(err)
	}
	secret := &tlsv3.Secret{}
	if err := resource.GetResource().UnmarshalTo(secret); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(secret.GetTlsCertificate().GetCertificateChain().GetInlineBytes())
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if resource.GetName() != "api.example.com" || cert.VerifyHostname("api.example.com") != nil || !cert.NotAfter.Equal(now.Add(leafLifetime)) {
		t.Fatalf("minted resource = %q, cert = %+v", resource.GetName(), cert)
	}
	if _, err := (&server{ca: ca, now: time.Now}).mint("*.example.com"); err == nil {
		t.Fatal("wildcard SNI was accepted")
	}
}
