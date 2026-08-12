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

package k8sjwt

import (
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestTokenReviewExtractsBoundIdentity(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if review.Spec.Token != "token" || len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != "atelet" {
			t.Fatalf("unexpected TokenReview spec: %+v", review.Spec)
		}
		return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
			Authenticated: true, Audiences: []string{"atelet"},
			User: authenticationv1.UserInfo{Username: "system:serviceaccount:workers:default", UID: "sa-uid", Extra: map[string]authenticationv1.ExtraValue{
				podNameExtra: {"worker-1"}, podUIDExtra: {"pod-uid"}, nodeNameExtra: {"node-1"}, nodeUIDExtra: {"node-uid"},
			}},
		}}, nil
	})
	claims, err := TokenReview(t.Context(), client, "token", "atelet")
	if err != nil {
		t.Fatal(err)
	}
	if claims.Namespace != "workers" || claims.ServiceAccountName != "default" || claims.PodUID != "pod-uid" || claims.NodeUID != "node-uid" {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestTokenReviewRejectsUnacceptedStatus(t *testing.T) {
	for _, status := range []authenticationv1.TokenReviewStatus{
		{Authenticated: false},
		{Authenticated: true, Audiences: []string{"other"}},
	} {
		client := fake.NewSimpleClientset()
		client.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, &authenticationv1.TokenReview{Status: status}, nil
		})
		if _, err := TokenReview(t.Context(), client, "token", "atelet"); err == nil {
			t.Fatalf("TokenReview accepted status %+v", status)
		}
	}
}
