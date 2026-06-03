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

package controlapi

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
)

func TestBuildAteletWorkloadSpecIncludesEgressPolicy(t *testing.T) {
	spec := buildAteletWorkloadSpec(&atev1alpha1.ActorTemplate{
		Spec: atev1alpha1.ActorTemplateSpec{
			PauseImage: "pause",
			Containers: []atev1alpha1.Container{
				{Name: "app", Image: "example/app"},
			},
			EgressPolicy: &atev1alpha1.EgressPolicy{
				DefaultAction: atev1alpha1.EgressPolicyActionDeny,
				Allow: []atev1alpha1.EgressPolicyRule{
					{
						To: []atev1alpha1.EgressPolicyDestination{
							{Host: "example.com"},
							{IPBlock: &atev1alpha1.EgressIPBlock{CIDR: "10.96.0.10/32"}},
						},
						Ports: []atev1alpha1.EgressPort{
							{Port: 443, Protocol: corev1.ProtocolTCP},
						},
						TLS: &atev1alpha1.EgressTLSPolicy{
							Mode:     atev1alpha1.EgressTLSModeIntercept,
							Required: true,
							Intercept: &atev1alpha1.EgressTLSInterceptPolicy{
								IssuerSecretRef: &corev1.SecretReference{
									Name:      "egress-ca",
									Namespace: "ate-system",
								},
								ValidateUpstream: true,
							},
						},
						Credentials: &atev1alpha1.EgressCredentialPolicy{
							Inject: []atev1alpha1.EgressCredentialInjection{
								{
									Header: "Authorization",
									ValueFrom: atev1alpha1.EgressCredentialValueFrom{
										SecretKeyRef: &corev1.SecretKeySelector{
											Key: "token",
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "github-token",
											},
										},
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
	allow := spec.GetEgressPolicy().GetAllow()
	if len(allow) != 1 {
		t.Fatalf("allow rule count = %d, want 1", len(allow))
	}
	if got, want := allow[0].GetTo()[0].GetHost(), "example.com"; got != want {
		t.Fatalf("host = %q, want %q", got, want)
	}
	if got, want := allow[0].GetTo()[1].GetCidr(), "10.96.0.10/32"; got != want {
		t.Fatalf("cidr = %q, want %q", got, want)
	}
	if got, want := allow[0].GetPorts()[0].GetProtocol(), "TCP"; got != want {
		t.Fatalf("protocol = %q, want %q", got, want)
	}
	if got, want := allow[0].GetTls().GetMode(), "Intercept"; got != want {
		t.Fatalf("tls mode = %q, want %q", got, want)
	}
	if got, want := allow[0].GetTls().GetIntercept().GetIssuerSecretRef().GetName(), "egress-ca"; got != want {
		t.Fatalf("issuer secret = %q, want %q", got, want)
	}
	if got, want := allow[0].GetCredentials().GetInject()[0].GetValueFrom().GetSecretKeyRef().GetName(), "github-token"; got != want {
		t.Fatalf("credential secret = %q, want %q", got, want)
	}
}
