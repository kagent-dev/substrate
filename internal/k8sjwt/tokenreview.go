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
	"context"
	"fmt"
	"slices"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	podNameExtra  = "authentication.kubernetes.io/pod-name"
	podUIDExtra   = "authentication.kubernetes.io/pod-uid"
	nodeNameExtra = "authentication.kubernetes.io/node-name"
	nodeUIDExtra  = "authentication.kubernetes.io/node-uid"
)

// TokenReview asks Kubernetes to authenticate a token and returns its live
// ServiceAccount and object-bound identity.
func TokenReview(ctx context.Context, client kubernetes.Interface, token, audience string) (*KubernetesClaims, error) {
	review, err := client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("TokenReview: %w", err)
	}
	if !review.Status.Authenticated || !slices.Contains(review.Status.Audiences, audience) {
		return nil, fmt.Errorf("token is not authenticated for the expected audience")
	}
	parts := strings.Split(review.Status.User.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return nil, fmt.Errorf("token is not a ServiceAccount token")
	}
	first := func(key string) string {
		if values := review.Status.User.Extra[key]; len(values) != 0 {
			return values[0]
		}
		return ""
	}
	return &KubernetesClaims{
		Subject: review.Status.User.Username, Namespace: parts[2], ServiceAccountName: parts[3], ServiceAccountUID: review.Status.User.UID,
		PodName: first(podNameExtra), PodUID: first(podUIDExtra), NodeName: first(nodeNameExtra), NodeUID: first(nodeUIDExtra),
	}, nil
}
