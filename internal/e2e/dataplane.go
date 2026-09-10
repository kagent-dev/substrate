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

package e2e

import (
	"context"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	routerDataplaneOnce  sync.Once
	routerIsAgentgateway bool
	routerDataplaneErr   error
)

// RouterIsAgentgateway reports whether the atenet-router Deployment runs the
// agentgateway dataplane. In that mode the pod has no Envoy and no
// atenet-router ext_proc process, so router-internal surfaces — the statusz
// page, atenet_router_* metrics, and Envoy's protocol mirroring to atunnel —
// do not exist. Suites gate assertions on those surfaces with this instead of
// a per-lane env knob: the deployed containers are the source of truth.
func RouterIsAgentgateway(ctx context.Context, t *testing.T) bool {
	t.Helper()
	routerDataplaneOnce.Do(func() {
		deploy, err := GetClients().K8s.AppsV1().Deployments(routerNamespace).Get(ctx, routerService, metav1.GetOptions{})
		if err != nil {
			routerDataplaneErr = err
			return
		}
		for _, c := range deploy.Spec.Template.Spec.Containers {
			if c.Name == "agentgateway" {
				routerIsAgentgateway = true
				return
			}
		}
	})
	if routerDataplaneErr != nil {
		t.Fatalf("detecting the router dataplane: %v", routerDataplaneErr)
	}
	return routerIsAgentgateway
}
