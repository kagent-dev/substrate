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

package main

import (
	"testing"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
)

func TestBuildAteomWorkloadSpecIncludesEgressPolicy(t *testing.T) {
	spec := buildAteomWorkloadSpec(&ateletpb.WorkloadSpec{
		Containers: []*ateletpb.Container{{Name: "app"}},
		EgressPolicy: &ateletpb.EgressPolicy{
			DefaultAction: "Deny",
			Allow: []*ateletpb.EgressPolicyRule{
				{
					To: []*ateletpb.EgressPolicyDestination{
						{Host: "example.com"},
					},
					Ports: []*ateletpb.EgressPort{
						{Port: 443, Protocol: "TCP"},
					},
					Tls: &ateletpb.EgressTLSPolicy{
						Mode:     "Intercept",
						Required: true,
						Intercept: &ateletpb.EgressTLSInterceptPolicy{
							IssuerSecretRef: &ateletpb.SecretReference{
								Name:      "egress-ca",
								Namespace: "ate-system",
							},
							ValidateUpstream: true,
						},
					},
					Credentials: &ateletpb.EgressCredentialPolicy{
						Inject: []*ateletpb.EgressCredentialInjection{
							{
								Header: "Authorization",
								ValueFrom: &ateletpb.EgressCredentialValueFrom{
									SecretKeyRef: &ateletpb.SecretKeySelector{
										Name: "github-token",
										Key:  "token",
									},
								},
							},
						},
					},
				},
			},
		},
	})

	if got, want := spec.GetEgressPolicy().GetDefaultAction(), "Deny"; got != want {
		t.Fatalf("default action = %q, want %q", got, want)
	}
	if got, want := spec.GetEgressPolicy().GetAllow()[0].GetTo()[0].GetHost(), "example.com"; got != want {
		t.Fatalf("host = %q, want %q", got, want)
	}
	if got, want := spec.GetEgressPolicy().GetAllow()[0].GetPorts()[0].GetPort(), uint32(443); got != want {
		t.Fatalf("port = %d, want %d", got, want)
	}
	if got, want := spec.GetEgressPolicy().GetAllow()[0].GetTls().GetMode(), "Intercept"; got != want {
		t.Fatalf("tls mode = %q, want %q", got, want)
	}
	if got, want := spec.GetEgressPolicy().GetAllow()[0].GetCredentials().GetInject()[0].GetHeader(), "Authorization"; got != want {
		t.Fatalf("credential header = %q, want %q", got, want)
	}
}
