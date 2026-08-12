// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCreateTokenModeTLSDoesNotOverwriteSecrets(t *testing.T) {
	tokenTLSNamespace, tokenTLSCASecret, tokenTLSSecret, tokenTLSCAConfigMap = "ate-system", "token-mode-ca", "token-mode-tls", "token-mode-ca"
	tokenTLSDNSNames = []string{"api.ate-system.svc", "*.valkey-cluster-service.ate-system.svc"}
	client := fake.NewSimpleClientset()
	if err := createTokenModeTLS(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	before, err := client.CoreV1().Secrets(tokenTLSNamespace).Get(t.Context(), tokenTLSSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Data["tls.crt"]) == 0 || len(before.Data["tls.key"]) == 0 || len(before.Data["credential-bundle.pem"]) == 0 {
		t.Fatalf("TLS Secret has incomplete data: %v", before.Data)
	}
	block, _ := pem.Decode(before.Data["tls.crt"])
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || cert.PublicKeyAlgorithm != x509.ECDSA {
		t.Fatalf("TLS certificate is not ECDSA: cert=%v err=%v", cert, err)
	}
	if err := createTokenModeTLS(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	after, err := client.CoreV1().Secrets(tokenTLSNamespace).Get(t.Context(), tokenTLSSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Data["tls.key"], after.Data["tls.key"]) {
		t.Fatal("existing TLS Secret was overwritten")
	}
}
