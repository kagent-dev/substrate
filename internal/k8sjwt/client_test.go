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

package k8sjwt

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDiscoveryTransportOnlySendsTokenToTrustedOrigins(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := &discoveryTransport{
		issuer: "https://issuer.example", apiServerHost: "10.0.0.1:443", tokenFile: tokenFile,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: req.Header.Clone()}, nil
		}),
	}
	for _, tc := range []struct {
		url, want string
	}{
		{"https://issuer.example/.well-known/openid-configuration", "Bearer secret"},
		{"https://10.0.0.1:443/openid/v1/jwks", "Bearer secret"},
		{"https://attacker.example/openid/v1/jwks", ""},
	} {
		req, _ := http.NewRequest(http.MethodGet, tc.url, nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Header.Get("Authorization"); got != tc.want {
			t.Errorf("%s authorization = %q, want %q", tc.url, got, tc.want)
		}
	}
}
