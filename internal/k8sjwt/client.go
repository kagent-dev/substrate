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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// InClusterDiscoveryClient returns an OIDC client that trusts caFile and only
// sends the discovery token to the configured issuer or Kubernetes JWKS path.
func InClusterDiscoveryClient(caFile, issuer, tokenFile string) (*http.Client, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading Kubernetes CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("kubernetes CA file %q contains no certificates", caFile)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &discoveryTransport{
			base:          &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}},
			issuer:        strings.TrimSuffix(issuer, "/"),
			apiServerHost: kubernetesServiceHost(),
			tokenFile:     tokenFile,
		},
	}, nil
}

type discoveryTransport struct {
	base          http.RoundTripper
	issuer        string
	apiServerHost string
	tokenFile     string
}

func (t *discoveryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	issuerURL, err := url.Parse(t.issuer)
	if err != nil {
		return nil, err
	}
	isJWKS := req.URL.Path == "/openid/v1/jwks" && t.apiServerHost != ""
	if req.URL.Scheme != issuerURL.Scheme || (req.URL.Host != issuerURL.Host && !isJWKS) {
		return t.base.RoundTrip(req)
	}
	token, err := os.ReadFile(t.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading discovery token: %w", err)
	}
	clone := req.Clone(req.Context())
	if isJWKS {
		// Kubernetes may advertise a node IP here. Route the fixed JWKS path
		// through the trusted API Service rather than sending a token to that host.
		clone.URL.Host = t.apiServerHost
	}
	clone.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	return t.base.RoundTrip(clone)
}

func kubernetesServiceHost() string {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	if host == "" || port == "" {
		return ""
	}
	return net.JoinHostPort(host, port)
}
