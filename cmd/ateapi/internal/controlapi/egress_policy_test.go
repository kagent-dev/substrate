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

package controlapi

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/principal"
	"github.com/agent-substrate/substrate/internal/proto/egresspolicypb"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateEgressPolicy(t *testing.T) {
	valid := &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "policy"},
		Target:   &ateapipb.EgressPolicy_Actor{Actor: &ateapipb.ObjectRef{Atespace: "team-a", Name: "actor"}},
		AllowAll: &emptypb.Empty{},
		Rules: []*ateapipb.EgressRule{
			{Match: &ateapipb.EgressRule_Hostname{Hostname: &ateapipb.HostnameMatch{Pattern: "*.example.com"}}},
			{Match: &ateapipb.EgressRule_IpBlocks{IpBlocks: &ateapipb.IPBlockMatch{Cidrs: []string{"192.0.2.0/24", "2001:db8::/32"}}}},
		},
	}
	if errs := validateEgressPolicy(valid); len(errs) != 0 {
		t.Fatalf("valid policy rejected: %v", errs)
	}
	mismatchedAtespace := normalizeEgressPolicy(valid)
	mismatchedAtespace.GetActor().Atespace = "team-b"
	if errs := validateEgressPolicy(mismatchedAtespace); len(errs) != 1 {
		t.Fatalf("cross-Atespace target errors = %v, want 1", errs)
	}

	invalid := normalizeEgressPolicy(valid)
	invalid.Rules[0].GetHostname().CredentialInjection = &ateapipb.HeaderCredentialInjection{
		Header: "Authorization", Credential: &ateapipb.CredentialReference{Name: "token"},
	}
	invalid.Rules[1].GetIpBlocks().Cidrs[0] = "192.0.2.1/24"
	if errs := validateEgressPolicy(invalid); len(errs) != 2 {
		t.Fatalf("invalid policy errors = %v, want wildcard-injection and noncanonical-CIDR errors", errs)
	}
}

func TestGetEffectiveEgressPolicy(t *testing.T) {
	tc := setupTest(t, namespaceForTest("egress-policy"))
	defer tc.cleanup()

	actor, err := tc.persistence.CreateActor(t.Context(), &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: testAtespace, Name: "egress-actor"},
		Status:   ateapipb.Actor_STATUS_RUNNING,
	})
	if err != nil {
		t.Fatal(err)
	}
	secretNamespace := namespaceForTest("egress-secret")
	if _, err := tc.k8sClient.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: secretNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.k8sClient.CoreV1().Secrets(secretNamespace).Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "api-token"},
		Data:       map[string][]byte{"authorization": []byte("Bearer resolved")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.service.CreateCredential(t.Context(), &ateapipb.CreateCredentialRequest{Credential: &ateapipb.Credential{
		Metadata: &ateapipb.ResourceMetadata{Atespace: testAtespace, Name: "api-token"},
		Source: &ateapipb.Credential_KubernetesSecret{KubernetesSecret: &ateapipb.KubernetesSecretKeySelector{
			Namespace: secretNamespace, Name: "api-token", Key: "authorization",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.service.CreateEgressPolicy(t.Context(), &ateapipb.CreateEgressPolicyRequest{EgressPolicy: &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: testAtespace, Name: "policy"},
		Target:   &ateapipb.EgressPolicy_Actor{Actor: &ateapipb.ObjectRef{Atespace: testAtespace, Name: "egress-actor"}},
		Rules: []*ateapipb.EgressRule{{Match: &ateapipb.EgressRule_Hostname{Hostname: &ateapipb.HostnameMatch{
			Pattern: "api.example.com",
			CredentialInjection: &ateapipb.HeaderCredentialInjection{
				Header: "Authorization", Credential: &ateapipb.CredentialReference{Name: "api-token"},
			},
		}}}},
	}}); err != nil {
		t.Fatal(err)
	}

	ctx := principal.InjectContext(context.Background(), principal.PrincipalInfo{Kind: principal.KindMTLS, ID: egressGatewayPrincipal})
	got, err := tc.service.GetEffectiveEgressPolicy(ctx, &egresspolicypb.GetEffectiveEgressPolicyRequest{
		Actor: &ateapipb.ObjectRef{Atespace: testAtespace, Name: "egress-actor"}, ActorUid: actor.GetMetadata().GetUid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetCredentials()) != 1 || string(got.GetCredentials()[0].GetValue()) != "Bearer resolved" {
		t.Fatalf("resolved credentials = %v", got.GetCredentials())
	}
	if _, err := tc.persistence.UpdateCredential(t.Context(), testAtespace, "api-token", func(credential *ateapipb.Credential) error {
		credential.Source = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.service.GetEffectiveEgressPolicy(ctx, &egresspolicypb.GetEffectiveEgressPolicyRequest{
		Actor: &ateapipb.ObjectRef{Atespace: testAtespace, Name: "egress-actor"}, ActorUid: actor.GetMetadata().GetUid(),
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("malformed credential status = %v, want FailedPrecondition", status.Code(err))
	}
	if _, err := tc.service.GetEffectiveEgressPolicy(context.Background(), &egresspolicypb.GetEffectiveEgressPolicyRequest{}); err == nil {
		t.Fatal("unauthenticated resolver call succeeded")
	}
}

type getActorErrorStore struct {
	store.Interface
	err error
}

func (s *getActorErrorStore) GetActor(context.Context, resources.ActorRef) (*ateapipb.Actor, error) {
	return nil, s.err
}

func TestGetEffectiveEgressPolicyActorLookupErrors(t *testing.T) {
	fixture := setupTest(t, namespaceForTest("egress-policy-errors"))
	defer fixture.cleanup()

	wrapped := &getActorErrorStore{Interface: fixture.persistence}
	fixture.service.persistence = wrapped
	ctx := principal.InjectContext(context.Background(), principal.PrincipalInfo{Kind: principal.KindMTLS, ID: egressGatewayPrincipal})
	req := &egresspolicypb.GetEffectiveEgressPolicyRequest{
		Actor: &ateapipb.ObjectRef{Atespace: testAtespace, Name: "actor"}, ActorUid: "uid",
	}
	for _, tc := range []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "missing actor", err: store.ErrNotFound, want: codes.PermissionDenied},
		{name: "persistence failure", err: errors.New("redis unavailable"), want: codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped.err = tc.err
			_, err := fixture.service.GetEffectiveEgressPolicy(ctx, req)
			if status.Code(err) != tc.want {
				t.Fatalf("status = %v, want %v", status.Code(err), tc.want)
			}
		})
	}
}

func TestCredentialHeaderValueValidation(t *testing.T) {
	for _, value := range [][]byte{[]byte("Bearer token"), {0x80, 0x81}} {
		if !validHeaderValue(value) {
			t.Errorf("validHeaderValue(%q) = false", value)
		}
	}
	for _, value := range [][]byte{[]byte("a\rb"), []byte("a\nb"), {'a', 0, 'b'}} {
		if validHeaderValue(value) {
			t.Errorf("validHeaderValue(%q) = true", value)
		}
	}
}
