//go:build linux

//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package agentgateway

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/proto/egresspb"
)

func TestRenderEgressAgentgatewayConfigAddsHTTPRouteAndHeaders(t *testing.T) {
	secrets := fakeEgressSecretResolver{
		values: map[string]string{
			"dev-agents/openai-token/token": "Bearer test-token",
		},
	}
	policy := egressPolicyForTest()
	policy.Allow[0].Ports[0].Port = 80
	policy.Allow[0].Tls = nil
	cfg, err := renderEgressAgentgatewayConfig(context.Background(), secrets, "dev-agents", t.TempDir(), policy)
	if err != nil {
		t.Fatalf("renderEgressAgentgatewayConfig() error = %v", err)
	}
	got := string(cfg)
	for _, want := range []string{
		"port: 15001",
		"name: allow-api-openai-com-80",
		"name: ':authority'",
		"exact: api.openai.com",
		"exact: api.openai.com:80",
		"host: api.openai.com:80",
		"Authorization: Bearer test-token",
		"directResponse:",
		"status: 403",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, got)
		}
	}
}

func TestRenderEgressAgentgatewayConfigDoesNotAddTLSRulesToHTTPListener(t *testing.T) {
	policy := egressPolicyForTest()
	policy.Allow[0].Credentials = nil
	cfg, err := renderEgressAgentgatewayConfig(context.Background(), nil, "dev-agents", t.TempDir(), policy)
	if err != nil {
		t.Fatalf("renderEgressAgentgatewayConfig() error = %v", err)
	}
	got := string(cfg)
	for _, notWant := range []string{
		"backendTLS: {}",
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("rendered HTTP listener config should not contain TLS route %q:\n%s", notWant, got)
		}
	}
}

func TestRenderEgressAgentgatewayConfigAddsTLSPassthroughTCPRoute(t *testing.T) {
	policy := egressPolicyForTest()
	policy.Allow[0].Credentials = nil
	cfg, err := renderEgressAgentgatewayConfig(context.Background(), nil, "dev-agents", t.TempDir(), policy)
	if err != nil {
		t.Fatalf("renderEgressAgentgatewayConfig() error = %v", err)
	}
	got := string(cfg)
	for _, want := range []string{
		"port: 15002",
		"protocol: TLS",
		"tcpRoutes:",
		"name: allow-api-openai-com-443",
		"hostnames:",
		"- api.openai.com",
		"host: api.openai.com:443",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, got)
		}
	}
}

func TestRenderEgressAgentgatewayConfigDefaultsPort443ToTLSPassthrough(t *testing.T) {
	policy := &egresspb.EgressPolicy{
		DefaultAction: "Deny",
		Allow: []*egresspb.EgressPolicyRule{{
			To: []*egresspb.EgressPolicyDestination{
				{Host: "example.com"},
			},
			Ports: []*egresspb.EgressPort{
				{Port: 80, Protocol: "TCP"},
				{Port: 443, Protocol: "TCP"},
			},
		}},
	}
	cfg, err := renderEgressAgentgatewayConfig(context.Background(), nil, "dev-agents", t.TempDir(), policy)
	if err != nil {
		t.Fatalf("renderEgressAgentgatewayConfig() error = %v", err)
	}
	got := string(cfg)
	for _, want := range []string{
		"port: 15001",
		"name: allow-example-com-80",
		"host: example.com:80",
		"port: 15002",
		"protocol: TLS",
		"name: allow-example-com-443",
		"hostnames:",
		"- example.com",
		"host: example.com:443",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, got)
		}
	}
}

func TestRenderEgressAgentgatewayConfigAddsInterceptHTTPSBind(t *testing.T) {
	secrets := fakeEgressSecretResolver{
		values: map[string]string{
			"dev-agents/openai-token/token": "Bearer test-token",
		},
		certs: map[string]fakeTLSSecret{
			"dev-agents/egress-ca": {
				cert: []byte("cert"),
				key:  []byte("key"),
			},
		},
	}
	policy := egressPolicyForTest()
	policy.Allow[0].Tls.Mode = "Intercept"
	policy.Allow[0].Tls.Intercept = &egresspb.EgressTLSInterceptPolicy{
		IssuerSecretRef: &egresspb.SecretReference{
			Name:      "egress-ca",
			Namespace: "dev-agents",
		},
		ValidateUpstream: true,
	}
	cfg, err := renderEgressAgentgatewayConfig(context.Background(), secrets, "dev-agents", t.TempDir(), policy)
	if err != nil {
		t.Fatalf("renderEgressAgentgatewayConfig() error = %v", err)
	}
	got := string(cfg)
	for _, want := range []string{
		"port: 15002",
		"protocol: HTTPS",
		"tls:",
		"mode: dynamicCa",
		"cert:",
		"intercept-ca.crt",
		"key:",
		"intercept-ca.key",
		"host: api.openai.com:443",
		"backendTLS:",
		"hostname: api.openai.com",
		"requestHeaderModifier:",
		"Authorization: Bearer test-token",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, got)
		}
	}
}

func TestRenderEgressAgentgatewayConfigCanDisableInterceptUpstreamValidation(t *testing.T) {
	secrets := fakeEgressSecretResolver{
		certs: map[string]fakeTLSSecret{
			"dev-agents/egress-ca": {
				cert: []byte("cert"),
				key:  []byte("key"),
			},
		},
	}
	policy := egressPolicyForTest()
	policy.Allow[0].Tls.Mode = "Intercept"
	policy.Allow[0].Tls.Intercept = &egresspb.EgressTLSInterceptPolicy{
		IssuerSecretRef: &egresspb.SecretReference{
			Name:      "egress-ca",
			Namespace: "dev-agents",
		},
		ValidateUpstream: false,
	}
	cfg, err := renderEgressAgentgatewayConfig(context.Background(), secrets, "dev-agents", t.TempDir(), policy)
	if err != nil {
		t.Fatalf("renderEgressAgentgatewayConfig() error = %v", err)
	}
	got := string(cfg)
	for _, want := range []string{
		"mode: dynamicCa",
		"backendTLS:",
		"insecure: true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, got)
		}
	}
}

func egressPolicyForTest() *egresspb.EgressPolicy {
	return &egresspb.EgressPolicy{
		DefaultAction: "Deny",
		Allow: []*egresspb.EgressPolicyRule{
			{
				To: []*egresspb.EgressPolicyDestination{
					{Host: "api.openai.com"},
				},
				Ports: []*egresspb.EgressPort{
					{Port: 443, Protocol: "TCP"},
				},
				Tls: &egresspb.EgressTLSPolicy{
					Mode:     "Require",
					Required: true,
				},
				Credentials: &egresspb.EgressCredentialPolicy{
					Inject: []*egresspb.EgressCredentialInjection{
						{
							Header: "Authorization",
							ValueFrom: &egresspb.EgressCredentialValueFrom{
								SecretKeyRef: &egresspb.SecretKeySelector{
									Name: "openai-token",
									Key:  "token",
								},
							},
						},
					},
				},
			},
		},
	}
}

type fakeEgressSecretResolver struct {
	values map[string]string
	certs  map[string]fakeTLSSecret
}

type fakeTLSSecret struct {
	cert []byte
	key  []byte
}

func (r fakeEgressSecretResolver) SecretValue(_ context.Context, namespace, name, key string) (string, error) {
	return r.values[namespace+"/"+name+"/"+key], nil
}

func (r fakeEgressSecretResolver) TLSSecret(_ context.Context, namespace, name string) ([]byte, []byte, error) {
	secret := r.certs[namespace+"/"+name]
	return secret.cert, secret.key, nil
}
