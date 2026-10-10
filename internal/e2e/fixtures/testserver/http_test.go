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
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHTTPHandlerDefaultsToEmptyHealthz(t *testing.T) {
	server := startOriginServer(t, newHTTPHandler("", ""), "", "")

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(server.httpURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading /healthz: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("GET /healthz body = %q, want empty", body)
	}
}

func TestHTTPSTestResponseUsesHTTP11AndVerifiedIdentity(t *testing.T) {
	cert := writeOriginCertificate(t)
	server := startOriginServer(t, newHTTPHandler("fixture response", ""), cert.certFile, cert.keyFile)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert.caPEM) {
		t.Fatal("adding fixture CA to trust pool")
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			Protocols: protocols,
			TLSClientConfig: &tls.Config{
				RootCAs:    roots,
				ServerName: "127.0.0.1",
			},
		},
	}
	response, err := client.Get(server.httpsURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over verified TLS: %v", err)
	}
	defer response.Body.Close()

	if response.Proto != "HTTP/1.1" {
		t.Errorf("HTTPS response protocol with HTTP/2 offered = %q, want HTTP/1.1", response.Proto)
	}
	if response.StatusCode != http.StatusOK {
		t.Errorf("HTTPS response status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading HTTPS response: %v", err)
	}
	if string(body) != "fixture response" {
		t.Errorf("HTTPS response body = %q, want %q", body, "fixture response")
	}
}

func TestCredentialHandler(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("expected-token"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		header string
		status int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong-token", http.StatusUnauthorized},
		{"Bearer expected-token", http.StatusNoContent},
	} {
		req := httptest.NewRequest(http.MethodGet, "/credential", nil)
		req.Header.Set("Authorization", tc.header)
		resp := httptest.NewRecorder()
		newHTTPHandler("fixture response", path).ServeHTTP(resp, req)
		if resp.Code != tc.status || resp.Body.Len() != 0 {
			t.Errorf("header %q: status=%d body=%q, want status=%d and no body", tc.header, resp.Code, resp.Body.String(), tc.status)
		}
	}
	resp := httptest.NewRecorder()
	credentialHandler(path+"-missing")(resp, httptest.NewRequest(http.MethodGet, "/credential", nil))
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("unreadable credential file: status=%d, want 500", resp.Code)
	}
}
