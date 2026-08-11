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

package ateredis

import (
	"errors"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func TestEgressPolicyActorIndex(t *testing.T) {
	_, s, ctx := setupTest(t)
	policy := testEgressPolicy("policy-a", "actor-a")
	created, err := s.CreateEgressPolicy(ctx, policy, "actor-uid-a")
	if err != nil {
		t.Fatalf("CreateEgressPolicy: %v", err)
	}
	if created.GetMetadata().GetUid() == "" || created.GetMetadata().GetVersion() != 1 {
		t.Fatalf("created metadata = %v", created.GetMetadata())
	}

	actorRef := resources.ActorRef{Atespace: testAtespace, Name: "actor-a"}
	got, err := s.GetEgressPolicyForActor(ctx, actorRef, "actor-uid-a")
	if err != nil || got.GetMetadata().GetName() != "policy-a" {
		t.Fatalf("GetEgressPolicyForActor = %v, %v", got, err)
	}
	if _, err := s.GetEgressPolicyForActor(ctx, actorRef, "replacement-uid"); !errors.Is(err, store.ErrUIDConflict) {
		t.Fatalf("stale Actor UID error = %v, want ErrUIDConflict", err)
	}
}

func TestEgressPolicyOnePerActor(t *testing.T) {
	_, s, ctx := setupTest(t)
	if _, err := s.CreateEgressPolicy(ctx, testEgressPolicy("policy-a", "actor-a"), "uid"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEgressPolicy(ctx, testEgressPolicy("policy-b", "actor-a"), "uid"); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("second policy error = %v, want ErrAlreadyExists", err)
	}
	if _, err := s.DeleteEgressPolicy(ctx, testAtespace, "policy-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEgressPolicy(ctx, testEgressPolicy("policy-b", "actor-a"), "uid"); err != nil {
		t.Fatalf("policy after delete: %v", err)
	}
}

func TestCredentialCRUD(t *testing.T) {
	_, s, ctx := setupTest(t)
	credential := &ateapipb.Credential{
		Metadata: &ateapipb.ResourceMetadata{Atespace: testAtespace, Name: "token"},
		Source: &ateapipb.Credential_KubernetesSecret{KubernetesSecret: &ateapipb.KubernetesSecretKeySelector{
			Namespace: "secrets", Name: "api", Key: "token",
		}},
	}
	_, err := s.CreateCredential(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateCredential(ctx, testAtespace, "token", func(current *ateapipb.Credential) error {
		current.GetKubernetesSecret().Key = "authorization"
		return nil
	})
	if err != nil || updated.GetKubernetesSecret().GetKey() != "authorization" || updated.GetMetadata().GetVersion() != 2 {
		t.Fatalf("UpdateCredential = %v, %v", updated, err)
	}
	listed, _, err := s.ListCredentials(ctx, testAtespace, 10, "")
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListCredentials = %v, %v", listed, err)
	}
}

func testEgressPolicy(name, actor string) *ateapipb.EgressPolicy {
	return &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: testAtespace, Name: name},
		Target:   &ateapipb.EgressPolicy_Actor{Actor: &ateapipb.ObjectRef{Atespace: testAtespace, Name: actor}},
		Rules: []*ateapipb.EgressRule{{
			Match: &ateapipb.EgressRule_Hostname{Hostname: &ateapipb.HostnameMatch{Pattern: "example.com"}},
		}},
	}
}
